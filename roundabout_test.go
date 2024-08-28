package roundabout

import (
	"testing"
)

// t.Log / t.Logf("%v", err)
// t.Error / t.Errorf,  mark fail and continue
// t.Fatal /  t.FatalF,  mark fail, exit

func TestRoundabout(t *testing.T) {
	b := Roundabout{}
	r1 := b.push(1)
	r2 := b.push(1)
	r3 := b.push(1)

	var done bool
	go func() {
		b.wait(r2)
		done = true
		b.pop(r2)

	}()

	b.wait(r1)
	b.pop(r1)

	b.wait(r3)
	b.pop(r3)
	if !done {
		t.Error("r2 not complete")
	}
}

func TestEnqueue(t *testing.T) {
	b := Roundabout{}
	go b.Signal(123, func() error { return nil })
	r1 := b.push(1)
	rX := b.push(10)
	rY := b.push(10)
	var r3 *rid

	var done bool
	go func() {
		b.Enqueue(1, func(uint16) error {
			r3 = b.push(1)
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

func BenchRoundabout(b *testing.B) {
	// setup
	b.ResetTimer()
	for range b.N {

	}
	// or b.RunParallel(func(pb *testing.PB) {})
}
