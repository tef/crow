package roundabout

import (
	"fmt"
	"math/bits"
	"strconv"
	"sync/atomic"
)

const width = 32

const (
	ZeroCell uint16 = iota
	FreeCell
	SpinCell
	SpinAllCell
)

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

// a slot in the roundabout

type slot struct {
	n      int
	epoch  uint16
	flags  uint16
	lane   uint32
	bitmap uint32
}

// a signal on the roundabout

type signal struct {
	epoch  uint16
	flags  uint16
	bitmap uint32
}

// Finally, a real data structure:

type Roundabout struct {
	header   atomic.Uint64     // <epoch:16> <flags:16> <bitmap: 32>
	cells    [32]atomic.Uint64 // <epoch:16> <state:16> <lane: 32>
	conflict func(uint32, uint32) bool
}

func (rb *Roundabout) String() string {
	h := unpack(rb.header.Load())
	return fmt.Sprintf("%v [%v] %v",
		strconv.FormatUint(uint64(h.body), 2),
		h.epoch,
		strconv.FormatUint(uint64(h.state), 2),
	)
}

func (rb *Roundabout) push(lane uint32, state uint16) (slot, bool) {
	header := rb.header.Load()

	h := unpack(header)

	n := int(h.epoch) % width
	var b uint32 = 1 << n

	if h.body&b == 0 {
		new_header := packed{h.epoch + 1, h.state, h.body | b}.pack()
		item := packed{h.epoch, state, lane}.pack()

		if rb.header.CompareAndSwap(header, new_header) {
			rb.cells[n].Store(item)
			e := slot{
				n:      n,
				epoch:  h.epoch,
				flags:  h.state,
				lane:   lane,
				bitmap: h.body,
			}
			return e, true

		}
	}

	return slot{}, false
}

func (rb *Roundabout) wait(r slot) {
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
			item := unpack(rb.cells[n].Load())
			if item.state == ZeroCell {
				// spin, uninitialised memory
				continue
			} else if item.epoch == epoch {
				// item has expected epoch of item in past
				// has been allocated on bitmap
				// check cell has been written

				if item.state == FreeCell {
					continue
				} else if item.state == SpinAll {
					continue
				}

				if rb.conflict == nil {
					if r.lane == item.body {
						continue
					}
				} else if rb.conflict(r.lane, item.body) {
					continue
				}
			}

			break
		}
	}
}

func (rb *Roundabout) pop(r slot) {
	// when we're done, we replace our cell with
	// an empty cell for the next value
	// so that waiting threads can skip
	// and newer waiting threads can pause
	// once it is allocated again

	next_item := packed{r.epoch + width, FreeCell, 0}.pack()
	rb.cells[r.n].Store(next_item)

	var b uint64 = 1 << r.n
	rb.header.And(^b) // go 1.23 needed
}

func (rb *Roundabout) setFence(flags uint16) (signal, bool) {
	header := rb.header.Load()
	h := unpack(header)

	if h.state&flags != 0 {
		// can't set flags, already set
		return signal{}, false
	}

	new_header := packed{h.epoch, h.state | flags, h.body}.pack()

	if rb.header.CompareAndSwap(header, new_header) {
		s := signal{
			epoch:  h.epoch,
			flags:  flags,
			bitmap: h.body,
		}
		return s, true
	}
	return signal{}, false
}

func (rb *Roundabout) spinFence(s signal) {
	if s.bitmap == 0 {
		return
	}

	// we check from epoch-32 to epoch-1
	epoch := s.epoch - uint16(32)
	n := int(s.epoch) % width

	// we shift the free bitmap so that epoch's cell is in the lsb
	// and epoch +1 is in next larger bit.
	bitmap := bits.RotateLeft32(s.bitmap, -n)

	// the free bitmap is a snapshot of where we were on allocation
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
			item := unpack(rb.cells[n].Load())
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

func (rb *Roundabout) clearFence(s signal) uint16 {
	// assumes you want the flags cleared
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

func (rb *Roundabout) SpinLockAll(fn func(uint16) error) error {
	for true {
		slot, ok := rb.push(0, SpinAllCell)
		if !ok {
			continue
		}

		rb.wait(slot)
		defer rb.pop(slot)

		return fn(slot.flags)
	}
	// huh
	return nil
}

func (rb *Roundabout) SpinLock(lane uint32, fn func(uint16) error) error {
	for true {
		slot, ok := rb.push(lane, SpinCell)
		if !ok {
			continue
		}

		rb.wait(slot)
		defer rb.pop(slot)

		return fn(slot.flags)
	}
	// huh
	return nil
}

func (rb *Roundabout) Signal(flags uint16, fn func() error) error {
	for true {
		signal, ok := rb.setFence(flags) // spins until flags are set
		if !ok {
			continue
		}

		rb.spinFence(signal)

		defer rb.clearFence(signal)
		return fn()
	}
	return nil
}
func (rb *Roundabout) Phase(flags uint16, fn func() error, after func(uint16, uint16) error) error {
	for true {
		signal, ok := rb.setFence(flags) // spins until flags are set
		if !ok {
			continue
		}

		rb.spinFence(signal)

		err := fn()
		end := rb.clearFence(signal)
		if err != nil {
			return err
		}
		return after(signal.epoch, end)
	}
	return nil
}
