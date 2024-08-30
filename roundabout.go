package roundabout

import (
	"fmt"
	"math/bits"
	"strconv"
	"sync/atomic"
)

const width = 32

// for packing and unpacking an int64 into three parts

type packed struct {
	epoch uint16
	state uint16
	body  uint32
}

func (c packed) pack() uint64 {
	return (uint64(c.epoch) << 48) | (uint64(c.state) << 32) | uint64(c.body)
}

func unpack(h uint64) packed {
	var epoch uint16 = uint16((h >> 48) & 65535)
	var state uint16 = uint16((h >> 32) & 65535)
	var body uint32 = uint32(h & 2147483647)
	return packed{epoch, state, body}
}

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
	- The epoch is the next free rb_entry
	- The bitfield tracks which items are allocated in the ring buffer
	- The flags are passed on to mutator threads allocating
- There's items of (epoch, state, lane) in the ring buffer itself:
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
	- Replace item with (epoch+width, free_state, 0)
	- This lets later writers skip the entry, or spin until it's allocated
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

This is why push/pop/etc aren't public methods.

*/

// roundabout cell states
const (
	ZeroCell uint16 = iota // unset memory
	FreeCell
	SpinCell
	SpinAllCell

	ReadCell
	ReadAllCell

	// We could also have AbortCell, AbortAllCell
	// to force later writers to abandon work

	// We could also have SpinPrefix or SpinLe / SpinGe
	// to vary key matching rules.

	// a <entry type><key type> encoding might go a long way
	// but there doesn't seem to be any genuine use cases
	// outside of "read/write" and "unallocated" / "uninitialised"
)

// a reserved rb_entry in the roundabout
type rb_entry struct {
	n      int
	epoch  uint16
	flags  uint16
	state  uint16
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
	log      [32]atomic.Uint64 // <epoch:16> <state:16> <lane: 32>
	Conflict func(uint32, uint32) bool
}

func (rb *Roundabout) Epoch() uint16 {
	h := unpack(rb.header.Load())
	return h.epoch
}

func (rb *Roundabout) Flags() uint16 {
	h := unpack(rb.header.Load())
	return h.state
}

func (rb *Roundabout) String() string {
	h := unpack(rb.header.Load())
	return fmt.Sprintf("%v [%v] %v",
		strconv.FormatUint(uint64(h.body), 2),
		h.epoch,
		strconv.FormatUint(uint64(h.state), 2),
	)
}

func (rb *Roundabout) Active(epoch uint32) bool {
	h := unpack(rb.header.Load())

	// only active epochs are e-32, e-31 ... e-1
	if epoch < (h.epoch - width) && epoch < h.epoch {
		return true
	if epoch > h.epoch && epoch < (h.epoch-width) {
		return true
	}

	e := int(rb.epoch) - 1
	bitmap := bits.RotateLeft32(int(e)%width)
	match := false
	for i := 0; i++; i < 32 {
		if e == int(epoch) {
			match = true
		}
		// don't need to read epoch values as if there was a 1
		// it was an earlier log entry
		if match {
			if b&1 == 1 {
				return false
			}
		}
		bitmap = bitmap >> 1
		e--
	}
	return true

}

// push a new item onto the log, with a given lane and state
// the state is "Spin" or "SpinAll", and the lane is usually
// some hash value

func (rb *Roundabout) push(lane uint32, state uint16) (rb_entry, bool) {
	header := rb.header.Load()

	h := unpack(header)

	n := int(h.epoch) % width
	var b uint32 = 1 << n

	if h.body&b == 0 {
		new_header := packed{h.epoch + 1, h.state, h.body | b}.pack()
		item := packed{h.epoch, state, lane}.pack()

		if rb.header.CompareAndSwap(header, new_header) {
			rb.log[n].Store(item)
			e := rb_entry{
				n:      n,
				epoch:  h.epoch,
				flags:  h.state,
				state:  state,
				lane:   lane,
				bitmap: h.body,
			}
			return e, true

		}
	}

	return rb_entry{}, false
}

// after allocating a rb_entry on the roundabout, we scan predecessors
// to find conflicts

func (rb *Roundabout) wait(r rb_entry) {
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
			item := unpack(rb.log[n].Load())
			if item.state == ZeroCell {
				// spin, uninitialised memory
				continue
			} else if item.epoch == epoch {
				// item has expected epoch of item in past
				// has been allocated on bitmap
				// check cell has been written

				if item.state == FreeCell {
					// the log entry has been allocated in the bitmap
					// but the thread has yet to write to it, so spin
					continue
				} else if r.state == SpinAllCell {
					// we're a SpinAll, we
					// wait for all predecessors
					continue
				} else if item.state == SpinAllCell {
					// we've met a spinall, so we wait for it
					continue
				} else if r.state == ReadAllCell {
					if item.state == ReadCell || item.state == ReadAllCell {
						break
					}
					// we're a readall, and we encounted a not-read
					// so spin
					continue
				} else if item.state == ReadAllCell {
					if r.state == ReadCell || r.state == ReadAllCell {
						break
					}
					// we're a a not-read, and we encounterd a read all
					// so spin
					continue

				} else if r.state == ReadCell && item.state == ReadCell {
					// ReadCells can only conflict with not-reads
					break
					// we only spin if the lane matches
				}

				if rb.Conflict == nil {
					if r.lane == item.body {
						continue
					}
				} else if rb.Conflict(r.lane, item.body) {
					continue
				}
			}

			break
		}
	}
}

// mark our work as complete, updating the item in the buffer
// before updating the header
func (rb *Roundabout) pop(r rb_entry) {
	next_item := packed{r.epoch + width, FreeCell, 0}.pack()
	rb.log[r.n].Store(next_item)

	var b uint64 = 1 << r.n
	rb.header.And(^b) // go 1.23 needed
}

// update the header in the buffer, so that all
// new mutators see flags

func (rb *Roundabout) setFence(flags uint16) (rb_fence, bool) {
	header := rb.header.Load()
	h := unpack(header)

	if h.state&flags != 0 {
		// can't set flags, already set
		return rb_fence{}, false
	}

	new_header := packed{h.epoch, h.state | flags, h.body}.pack()

	if rb.header.CompareAndSwap(header, new_header) {
		s := rb_fence{
			epoch:     h.epoch,
			flags:     flags,
			new_flags: h.state | flags,
			bitmap:    h.body,
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
			item := unpack(rb.log[n].Load())
			if item.state == ZeroCell {
				// spin, uninitialised memory
				continue
			} else if item.epoch == epoch {
				// spin, predecessor still active
				continue
			}

			break
		}
		epoch++
		bitmap = bitmap >> 1
	}
}

// clear out flags, OR'ing out our changes
// and again, only affecting new writers

func (rb *Roundabout) clearFence(s rb_fence) uint16 {
	for true {
		header := rb.header.Load()
		h := unpack(header)

		new_header := packed{h.epoch, h.state ^ s.flags, h.body}.pack()

		if rb.header.CompareAndSwap(header, new_header) {
			return h.epoch
		}
	}
	return 0

}

// run the callback when no other callbacks with the same lane are active
func (rb *Roundabout) SpinLock(lane uint32, fn func(uint16, uint16) error) error {
	for true {
		rb_entry, ok := rb.push(lane, SpinCell)
		// XXX could count the spins here
		// and park the thread

		if !ok {
			continue
		}

		rb.wait(rb_entry)
		defer rb.pop(rb_entry)

		return fn(rb_entry.epoch, rb_entry.flags)
	}
	// huh
	return nil
}

// run the callback once all other callbacks have ended, regardless of lane
func (rb *Roundabout) SpinLockAll(fn func(uint16, uint16) error) error {
	for true {
		rb_entry, ok := rb.push(0, SpinAllCell)
		if !ok {
			continue
		}

		rb.wait(rb_entry)
		defer rb.pop(rb_entry)
		// maybe think about passing in epoch and flags
		return fn(rb_entry.epoch, rb_entry.flags)
	}
	// huh
	return nil
}

// run the callback when no other callbacks with the same lane are active
// except other readers
func (rb *Roundabout) SpinRead(lane uint32, fn func(uint16, uint16) error) error {
	for true {
		rb_entry, ok := rb.push(lane, ReadCell)
		if !ok {
			continue
		}

		rb.wait(rb_entry)
		defer rb.pop(rb_entry)

		return fn(rb_entry.epoch, rb_entry.flags)
	}
	// huh
	return nil
}

// run the callback when no other callbacks are running, whatever lane
// except other readers
func (rb *Roundabout) SpinReadAll(fn func(uint16, uint16) error) error {
	for true {
		rb_entry, ok := rb.push(0, ReadAllCell)
		if !ok {
			continue
		}

		rb.wait(rb_entry)
		defer rb.pop(rb_entry)

		return fn(rb_entry.epoch, rb_entry.flags)
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
