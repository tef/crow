package crow

import (
	"fmt"
	"math/bits"
	"strconv"
	"sync/atomic"
)

const width = 32

/*
A roundabout is effectively an in-memory write-ahead log:

- Threads publish their planned operation to the log
- Threads scan the log for all active predecessors, and spin on conflicts
- Once complete, threads remove their entries from the log

This allows a roundabout to be used for mutual exclusion, as well as coordination between threads:

- A thread can publish an item that blocks all subsequent threads.
- A thread can also publish an item that only conflicts with writers.
- A thread is given a (epoch, flag) pair after allocating, which can be used to order operations.
- Flags allow threads to advise active threads of operations in progress, without taking up room on the log
- Flags can be set for all new theads, and a thread can wait for old writers to complete.

Internally, a roundabout is just a fancy ring buffer:

- There's a header of (epoch, flags, bitfield32)
	- The epoch is the next free rb_cell
	- The bitfield tracks which items are allocated in the ring buffer
	- The flags are passed on to mutator threads allocating
- There's items of (epoch, kind, lane) in the ring buffer itself:
	- The epoch lets us know if an item comes before or after us. A generational index by any other name.
	- State lets us know if it's a special lane (like an exclusive lock)
	- Key lets us find conflicting items for regular threads

The operations are pretty much what you'd do for a ringbuffer, but
with a bitfield free-list:

- Insertion is
	- Check epoch+1's bit in the bitfield
	- If 0, CAS in a new header with epoch+1 and the bitfield updated
- Scanning is
	- With the bitfield from allocation, scan the ring buffer
	- If the epoch is what we expect for an earlier item, check it
	- Spin if there's a conflict
- Freeing is
	- Replace item with (epoch+width, free_kind, 0)
	- This lets later writers skip the cell, or spin until it's allocated
	- CAS in a new header with the bitfield updated

This allows a roundabout to be used in a number of different ways:

- Like a fine grained lock
	- Each thread inserts a 32bit lane, and spins if there's a match
- Like a single, big lock
	- The mutator threads spin until all predecessors are complete
	- Succesor threads spin when encountering a big lock in the log
- Like a reader, writer lock
	- Readers create conflict free log items
	- Only writers force mutual exclusion
- Like an optimistic read lock
	- Checking the epoch before and after reads
- Like RCU
	- A fence can be used to notify all future writers an operation is in progress
	- A fence can wait for all earlier writers to exit before starting work
	- Can be used to handle concurrent resizes or snapshots, without blocking writers
- Like ESBR
	- Can note down the epoch when a structure is retired
	- Can check if epoch has advanced, or all earlier writers have exited
	- Can be used to reclaim shared structures, or keep thread local free lists

Not bad for a ring buffer, frankly.

The one major downside? If a thread tries to obtain multiple entries on the log,
it might succeed, but under contention it will lock hard. The only way to safely
acquire multiple entries on the ring buffer is atomically.

This is why push/pop/etc aren't public methods. A thread shouldn't nest calls
to SpinLock etc but our hands are tied in go, alas.


*/

// roundabout cell kind, could be reorged to allow easy bitfield testing
const (
	ZeroCell    uint16 = iota // unitialised memory, all 0
	PendingCell               // epoch set, kind pending

	ShareLane // Blocks on Locks, ignores Order and Share in lane
	ShareRing  // Blocks on Locks, ignores Order and Share in ring

	OrderLane // Blocks on any Lock, Order in lane, ignores Share
	OrderRing  // Blocks on any Lock, Order in ring, ignores Share

	LockLane // Blocks on any predecessors in lane
	LockRing  // Blocks on all predecessors in ring

	/*
		There is room for other behaviours, but a user
		can override lane matching behaviour with a function

		In theory, we could make an entry that tells future
		workers to abort, but flags already handle that case

		we could encode this as <pending><shared><ordered><locked><lane/ring>
		and speed up comparisons and checks but big meh
	*/
)

// the header of the ring buffer

type Header struct {
	epoch  uint16
	flags  uint16
	bitmap uint32
}

func (h Header) pack() uint64 {
	return (uint64(h.epoch) << 48) | (uint64(h.flags) << 32) | uint64(h.bitmap)
}

func unpackHeader(h uint64) Header {
	var epoch uint16 = uint16((h >> 48) & 65535)
	var flags uint16 = uint16((h >> 32) & 65535)
	var bitmap uint32 = uint32(h & 2147483647)
	return Header{epoch, flags, bitmap}
}

// the entries in the ring buffers

type Cell struct {
	epoch uint16
	kind  uint16
	lane  uint32
}

func (c Cell) pack() uint64 {
	return (uint64(c.epoch) << 48) | (uint64(c.kind) << 32) | uint64(c.lane)
}

func unpackCell(h uint64) Cell {
	var epoch uint16 = uint16((h >> 48) & 65535)
	var kind uint16 = uint16((h >> 32) & 65535)
	var lane uint32 = uint32(h & 2147483647)
	return Cell{epoch, kind, lane}
}

// a cell in use in the roundabout
type rb_cell struct {
	n      int
	epoch  uint16
	flags  uint16
	kind   uint16
	lane   uint32
	bitmap uint32
}

// a change to the headers
type rb_fence struct {
	epoch     uint16
	flags     uint16
	new_flags uint16
	bitmap    uint32
}

// and the actual structure itself:
// a ring buffer of log entries, and a header including epoch and freelist

type Roundabout struct {
	header   atomic.Uint64     // <epoch:16> <flags:16> <bitmap: 32>
	log      [32]atomic.Uint64 // <epoch:16> <kind:16> <lane: 32>
	Conflict func(uint32, uint32) bool
}

// before you ask, yes, 32 isn't a lot of elements, but it is currently a lot of cpus
// we could build a larger roundabout from a linked list/free list, or we could 
// partition a larger ring into 32 buckets, give each one a bitmap, 
// and do some special dancing to ensure we don't get a race from updating the header
// + the bitmap at the same time

func (rb *Roundabout) Epoch() uint16 {
	h := unpackHeader(rb.header.Load())
	return h.epoch
}

func (rb *Roundabout) Flags() uint16 {
	h := unpackHeader(rb.header.Load())
	return h.flags
}

func (rb *Roundabout) String() string {
	h := unpackHeader(rb.header.Load())
	return fmt.Sprintf("%v [%v] %v",
		strconv.FormatUint(uint64(h.bitmap), 2),
		h.epoch,
		strconv.FormatUint(uint64(h.flags), 2),
	)
}

func (rb *Roundabout) Active(epoch uint16) bool {
	h := unpackHeader(rb.header.Load())

	if h.epoch == epoch {
		return h.bitmap == 0
	}
	
	// if we're within width bits, epoch could have
	// active predecessors

	// XXX could create a 1111111 bit, << diff, then rot it by epoch
	// and just AND it with header

	diff := h.epoch - epoch

	if diff < 0 || diff >= width {
		return false
	}

	// skim off all bits of jobs ahead of epoch given
	bitmap := bits.RotateLeft32(h.bitmap, int(h.epoch-1)%width)
	bitmap = bitmap >> diff

	for i := 0; i < width-int(diff); i++ {
		if bitmap&1 == 1 {
			return true
		}
		bitmap = bitmap >> 1
	}
	return false

}

// push a new item onto the log, with a given lane and kind
// the kind is "Spin" or "SpinRing", and the lane is usually
// some hash value

func (rb *Roundabout) push(lane uint32, kind uint16) (rb_cell, bool) {
	header := rb.header.Load()

	h := unpackHeader(header)

	n := int(h.epoch) % width
	var b uint32 = 1 << n

	if h.bitmap&b == 0 {
		new_header := Header{h.epoch + 1, h.flags, h.bitmap | b}.pack()
		item := Cell{h.epoch, kind, lane}.pack()

		if rb.header.CompareAndSwap(header, new_header) {
			rb.log[n].Store(item)
			e := rb_cell{
				n:      n,
				epoch:  h.epoch,
				flags:  h.flags,
				kind:   kind,
				lane:   lane,
				bitmap: h.bitmap,
			}
			return e, true

		}
	}

	return rb_cell{}, false
}

// after allocating a rb_cell on the roundabout, we scan predecessors
// to find conflicts

func (rb *Roundabout) wait(r rb_cell) {
	// n.b we will never scan epoch -32 to 0 for the first cycle
	// as the bitmap in the header is all zeros

	if r.bitmap == 0 {
		return
	}

	// we check from epoch-31 to epoch-1
	epoch := r.epoch - uint16(32)

	// we shift the free bitmap so that our cell is in the lsb
	bitmap := bits.RotateLeft32(r.bitmap, -r.n)

	// the free bitmap is a snapshot of where we were on allocation
	// so will not include any items ahead of us

	for i := 0; i < 31; i++ {
		epoch++
		bitmap = bitmap >> 1
		if bitmap&1 == 0 { // free space
			continue
		}
		// fmt.Println(r.epoch,":", epoch, bitmap&1)

		n := int(epoch) % width
		for true {
			item := unpackCell(rb.log[n].Load())
			if item.kind == ZeroCell {
				// spin, uninitialised memory
				continue
			} else if item.epoch == epoch {
				// item has expected epoch of item in past
				// has been allocated on bitmap
				// check cell has been written

				if item.kind == PendingCell {
					// the log cell has been allocated in the bitmap
					// but the thread has yet to write to it, so spin
					continue
				}

				if r.kind == LockRing || item.kind == LockRing {
					// we wait for all predecessors
					continue
				} else if r.kind == OrderRing {
					// atomics not blocked by reads
					if item.kind == ShareLane || item.kind == ShareRing {
						break
					}
					// we block on all Lock, Order predecessors
					// and atomics
					continue

				} else if r.kind == ShareRing {
					// we block when we see a Lock, but not Share or Atomics
					if item.kind == LockLane || item.kind == LockRing {
						continue
					}
					break
				} else if r.kind == LockLane {
					// block on all wide actions
					if item.kind == LockRing || item.kind == OrderRing || item.kind == ShareRing {
						continue
					}
					// check lane below for LockLane, OrderLane, ShareLane

				} else if r.kind == OrderLane {
					// block on all wide actions, except reads
					if item.kind == LockRing || item.kind == OrderRing {
						continue
					}
					// ignore reads
					if item.kind == ShareLane || item.kind == ShareRing {
						break
					}
					// check lane for LockLane, OrderLane

				} else if r.kind == ShareLane {
					// blocked by any Lock
					if item.kind == LockRing {
						continue
					}
					// ignores atomics, reads
					if item.kind == OrderLane || item.kind == OrderRing {
						break
					}
					if item.kind == ShareLane || item.kind == ShareRing {
						break
					}
					// check lane for LockLane below
				}
				// if we're a Lock lane, we chec Lock, atomic, read lane here
				// if we're an atomic lane, we chec Lock, atomic lane here
				// if we're a read lane, we chec Lock lane here

				if rb.Conflict == nil {
					if r.lane == item.lane {
						continue
					}
				} else if rb.Conflict(r.lane, item.lane) {
					continue
				}
			}

			break
		}
	}
}

// mark our work as complete, updating the item in the buffer
// before updating the header
func (rb *Roundabout) pop(r rb_cell) {
	next_item := Cell{r.epoch + width, PendingCell, 0}.pack()
	rb.log[r.n].Store(next_item)

	var b uint64 = 1 << r.n
	rb.header.And(^b) // go 1.23 needed
}

// update the header in the buffer, so that all
// new mutators see flags

func (rb *Roundabout) setFence(flags uint16) (rb_fence, bool) {
	header := rb.header.Load()
	h := unpackHeader(header)

	if h.flags&flags != 0 {
		// can't set flags, already set
		return rb_fence{}, false
	}

	new_header := Header{h.epoch, h.flags | flags, h.bitmap}.pack()

	if rb.header.CompareAndSwap(header, new_header) {
		s := rb_fence{
			epoch:     h.epoch,
			flags:     flags,
			new_flags: h.flags | flags,
			bitmap:    h.bitmap,
		}
		return s, true
	}
	return rb_fence{}, false
}

// now that we've update the header, we wait for
// all earlier work to complete

func (rb *Roundabout) spinFence(s rb_fence) {
	if s.bitmap == 0 {
		return
	}

	// there's no allocation made for flag changes
	// so we check from epoch-32 to epoch-1

	epoch := s.epoch - uint16(32)
	n := int(s.epoch) % width

	// we shift the free bitmap so that epoch's cell is in the lsb
	// and epoch +1 is in next larger bit.
	bitmap := bits.RotateLeft32(s.bitmap, -n)

	// the free bitmap is a snapshot of where we were on header update
	// so will not include any items ahead of us

	for i := 0; i < 32; i++ {
		if bitmap&1 == 0 { // free space
			epoch++
			bitmap = bitmap >> 1
			continue
		}
		// fmt.Println(s.epoch,":", epoch, bitmap&1)

		n := int(epoch) % width
		for true {
			item := unpackCell(rb.log[n].Load())
			if item.kind == ZeroCell {
				// spin, uninitialised memory
				continue
			} else if item.epoch == epoch {
				// spin, predecessor still active
				// unless it's a read, which we can ignore
				// may want to have diff fence or spinWriters
				// but cant think of why we'd need a fence that waits
				// for old readers that wouldn't be a LockRing

				if item.kind == ShareLane || item.kind == ShareRing {
					break
				}
				continue
			}

			break
		}
		epoch++
		bitmap = bitmap >> 1
	}
}

// clear out flags, OR'ing out our changes
// and again, only affecting new threads

func (rb *Roundabout) clearFence(s rb_fence) uint16 {
	for true {
		header := rb.header.Load()
		h := unpackHeader(header)

		new_header := Header{h.epoch, h.flags ^ s.flags, h.bitmap}.pack()

		if rb.header.CompareAndSwap(header, new_header) {
			return h.epoch
		}
	}
	return 0

}

// run the callback once all other callbacks have ended, regardless of lane
func (rb *Roundabout) LockRing(fn func(uint16, uint16) error) error {
	for true {
		rb_cell, ok := rb.push(0, LockRing)
		if !ok {
			continue
		}

		rb.wait(rb_cell)
		defer rb.pop(rb_cell)
		// maybe think about passing in epoch and flags
		return fn(rb_cell.epoch, rb_cell.flags)
	}
	// huh
	return nil
}

// run the callback once all Locked, Order callbacks have ended, regardless of lane
func (rb *Roundabout) OrderRing(fn func(uint16, uint16) error) error {
	for true {
		rb_cell, ok := rb.push(0, OrderRing)
		if !ok {
			continue
		}

		rb.wait(rb_cell)
		defer rb.pop(rb_cell)
		// maybe think about passing in epoch and flags
		return fn(rb_cell.epoch, rb_cell.flags)
	}
	// huh
	return nil
}

// run the callback once all Locked callbacks are over, whatever lane
func (rb *Roundabout) ShareRing(fn func(uint16, uint16) error) error {
	for true {
		rb_cell, ok := rb.push(0, ShareRing)
		if !ok {
			continue
		}

		rb.wait(rb_cell)
		defer rb.pop(rb_cell)

		return fn(rb_cell.epoch, rb_cell.flags)
	}
	// huh
	return nil
}

// run the callback once all other callbacks with the same lane are over
func (rb *Roundabout) LockLane(lane uint32, fn func(uint16, uint16) error) error {
	for true {
		rb_cell, ok := rb.push(lane, LockLane)
		// XXX could count the spins here
		// and park the thread

		if !ok {
			continue
		}

		rb.wait(rb_cell)
		defer rb.pop(rb_cell)

		return fn(rb_cell.epoch, rb_cell.flags)
	}
	// huh
	return nil
}

// run the callback when no other Locked, Order callbacks with the same lane are active
func (rb *Roundabout) OrderLane(lane uint32, fn func(uint16, uint16) error) error {
	for true {
		rb_cell, ok := rb.push(lane, OrderLane)
		// XXX could count the spins here
		// and park the thread

		if !ok {
			continue
		}

		rb.wait(rb_cell)
		defer rb.pop(rb_cell)

		return fn(rb_cell.epoch, rb_cell.flags)
	}
	// huh
	return nil
}

// run the callback when no Locked with the same lane are active
func (rb *Roundabout) ShareLane(lane uint32, fn func(uint16, uint16) error) error {
	for true {
		rb_cell, ok := rb.push(lane, ShareLane)
		if !ok {
			continue
		}

		rb.wait(rb_cell)
		defer rb.pop(rb_cell)

		return fn(rb_cell.epoch, rb_cell.flags)
	}
	// huh
	return nil
}

// update these flags, run the callback, clear the flags
func (rb *Roundabout) Fence(flags uint16, fn func(uint16, uint16) error) error {
	for true {
		rb_fence, ok := rb.setFence(flags) // spins until flags are set
		if !ok {
			continue
		}

		rb.spinFence(rb_fence)

		defer rb.clearFence(rb_fence)
		return fn(rb_fence.epoch, rb_fence.new_flags)
	}
	return nil
}

// update the flags, run the first callback,
// clear the flags, run the second callback

func (rb *Roundabout) Phase(flags uint16, fn func(uint16, uint16) error, after func(uint16, uint16) error) error {
	for true {
		rb_fence, ok := rb.setFence(flags) // spins until flags are set
		if !ok {
			continue
		}

		rb.spinFence(rb_fence)

		err := fn(rb_fence.epoch, rb_fence.new_flags)
		end := rb.clearFence(rb_fence)
		if err != nil {
			return err
		}
		return after(rb_fence.epoch, end)
	}
	return nil
}
