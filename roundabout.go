package roundabout

import (
	"fmt"
	"math/bits"
	"strconv"
	"sync/atomic"
)

// get rid of rid
// flag for number

type rid struct {
	n      int
	bitmap uint32

	epoch uint16
	flags uint16
	key   uint32
}

// split4? maybe a type alias for cell and header

func split3(h uint64) (uint16, uint16, uint32) {
	var epoch uint16 = uint16((h >> 48) & 65535)
	var flags uint16 = uint16((h >> 32) & 65535)
	var body uint32 = uint32(h & 2147483647)
	return epoch, flags, body
}

func join3(epoch uint16, flags uint16, body uint32) uint64 {
	return (uint64(epoch) << 48) | (uint64(flags) << 32) | uint64(body)
}

type Roundabout struct {
	// <epoch:16> <flags:16> <bitmap: 32>
	header atomic.Uint64
	// <epoch:16> <flags:16> <op
	cells [32]atomic.Uint64

	conflict func(uint32, uint32) bool
}

func (br *Roundabout) String() string {
	h := br.header.Load()

	epoch, flags, bitmap := split3(h)

	return fmt.Sprintf("%v [%v] %v",
		strconv.FormatUint(uint64(bitmap), 2),
		epoch,
		strconv.FormatUint(uint64(flags), 2),
	)
}

func (br *Roundabout) Enqueue(key uint32, fn func(uint16) error) error {
	for true {
		rid := br.push(key)
		if rid == nil {
			continue
		}

		br.wait(rid)
		defer br.pop(rid)

		return fn(rid.flags)
	}
	// huh
	return nil
}

func (br *Roundabout) pushFlags(flags uint16) {

}

func (br *Roundabout) popFlags(flags uint16) {

}

func (br *Roundabout) push(key uint32) *rid {
	width := 32
	h := br.header.Load()

	epoch, flags, bitmap := split3(h)

	n := int(epoch) % width
	var b uint32 = 1 << n

	// we set the lsb of flags, so that
	// the insertion into the cell is marked
	// as complete

	h2 := join3(epoch+1, flags|1, bitmap|b)

	i := join3(epoch, flags|1, key)

	if bitmap&b == 0 {
		if br.header.CompareAndSwap(h, h2) {
			br.cells[n].Store(i)
			rid := &rid{
				bitmap: bitmap,
				n:      n,
				epoch:  epoch,
				flags:  flags,
				key:    key,
			}
			return rid

		}
	}

	return nil
}

func (br *Roundabout) wait(r *rid) {
	if r.bitmap == 0 {
		return
	}
	width := 32

	// n.b we will never scan epoch -32 to 0 for the first cycle
	// as the bitmap in the header is all zeros

	// as we mark work as done by incrementing epoch and clearing flags
	// every cell will have a valid epoch

	// we check from epoch-32 to epoch-1
	epoch := r.epoch - uint16(32)

	// we shift the free bitmap so that our cell is in the lsb
	bitmap := bits.RotateLeft32(r.bitmap, -r.n)

	// the free bitmap is a snapshot of where we were on allocation
	// so will not include any items ahead of us

	for i := 0; i < 31; i++ {
		epoch++
		bitmap = bitmap >> 1
		if bitmap&1 == 0 {
			continue
		}

		n := int(epoch) % width
		for true {
			item := br.cells[n].Load()
			item_epoch, item_flags, item_key := split3(item)

			// again, as we will only ever check cells that
			// have been written to at least once, they
			// will all have a valid epoch
			// and only have flags cleared on removal

			if item_epoch == epoch {
				// item has expected epoch of item in past
				if item_flags == 0 {
					// not initialised yet, spin
					continue
				}

				// we have an item that precedes us
				// with a valid key

				if br.conflict == nil {
					if r.key == item_key {
						continue
					}
				} else if br.conflict(r.key, item_key) {
					continue
				}
			}

			break
		}
	}

}

func (br *Roundabout) pop(r *rid) {
	// when we're done, we replace our cell with
	// an empty cell for the next value
	// so that waiting threads can skip
	// and newer waiting threads can pause
	// once it is allocated again

	next_item := join3(r.epoch+32, 0, 0)
	br.cells[r.n].Store(next_item)

	var b uint64 = 1 << r.n
	br.header.And(^b) // go 1.23 needed
}
