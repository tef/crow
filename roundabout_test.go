package roundabout

import (
	"testing"
)

// t.Log / t.Logf("%v", err)
// t.Error / t.Errorf,  mark fail and continue
// t.Fatal /  t.FatalF,  mark fail, exit

func TestRoundabout(t *testing.T) {
	b2 := Roundabout{}
	start, _ := b2.push(1001, SpinCell)
	ended, _ := b2.push(1001, SpinCell)

	b := Roundabout{}
	t.Log(b.String())

	go func() {
		t.Log("popping")
		b.Phase(123, func(epoch uint16, flags uint16) error {
			t.Log("in phase start", b.String())
			b2.pop(start)
			b.push(1111, SpinCell)
			return nil
		}, func(start, end uint16) error {
			t.Log("from", start, "to", end)
			return nil
		})
	}()
	r1, _ := b.push(1, SpinCell)
	r2, _ := b.push(1, SpinCell)
	r3, _ := b.push(1, SpinCell)
	t.Log(b.String())

	var done bool
	go func() {
		b.wait(r2)
		done = true
		b.pop(r2)

	}()

	b.wait(r1)
	b.pop(r1)
	t.Log("pop", b.String())

	b.wait(r3)
	b.pop(r3)
	if !done {
		t.Error("r2 not complete")
	}
	t.Log("waiting for first three ended", b2.String())

	b2.wait(ended)
	t.Log(b.String())
}

func TestSpinLock(t *testing.T) {
	b := Roundabout{}
	r1, _ := b.push(1, SpinCell)
	rX, _ := b.push(10, SpinCell)
	rY, _ := b.push(10, SpinCell)
	var r3 rb_cell

	var done bool
	go func() {
		b.SpinLock(1, func(uint16, uint16) error {
			r3, _ = b.push(1, SpinCell)
			b.pop(rX)
			done = true
			return nil
		})

	}()

	b.wait(r1)
	b.pop(r1)

	// enqueue should run, setting r3,
	// clearing rX, which blocks rY
	b.wait(rY)
	b.pop(rY)

	b.wait(r3)
	b.pop(r3)
	if !done {
		t.Error("r2 not complete")
	}
}

func TestSpinLockAll(t *testing.T) {
	rb := Roundabout{}

	rb1, _ := rb.push(1, SpinCell)
	rb2, _ := rb.push(1, SpinCell)

	b := Roundabout{}
	r1, _ := b.push(1, SpinCell)
	r2, _ := b.push(2, SpinCell)

	var done bool
	go func() {
		b.SpinLock(1, func(uint16, uint16) error {
			t.Log("in lock")
			done = true
			rb.pop(rb1)
			return nil
		})

	}()

	b.wait(r1)
	b.pop(r1)
	b.wait(r2)
	b.pop(r2)
	t.Log("waiting on rb2")

	rb.wait(rb2)
	rb.pop(rb2)

	if !done {
		t.Error("r2 not complete")
	}
}

func BenchRoundabout(b *testing.B) {
	// setup
	b.ResetTimer()
	for range b.N {

	}
	// or b.RunParallel(func(pb *testing.PB) {})
}
