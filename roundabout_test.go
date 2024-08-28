package roundabout

import (
	"testing"
)

// t.Log / t.Logf("%v", err)
// t.Error / t.Errorf,  mark fail and continue
// t.Fatal /  t.FatalF,  mark fail, exit

func TestRoundabout(t *testing.T) {
	b := Roundabout{}
	b.init()
	t.Log(b.String())
	r1, _ := b.push(1)
	t.Log(b.String())
	r2, _ := b.push(1)
	t.Log(b.String())
	r3, _ := b.push(1)
	t.Log(b.String())

	var done bool
	go func() {
		b.wait(r2)
		done = true
		b.pop(r2)

	}()

	b.wait(r1)
	b.pop(r1)
	t.Log(b.String())

	b.wait(r3)
	b.pop(r3)
	if !done {
		t.Error("r2 not complete")
	}
}

func TestEnqueue(t *testing.T) {
	b := Roundabout{}
	b.init()
	go b.Signal(123, func() error { return nil })
	r1, _ := b.push(1)
	rX, _ := b.push(10)
	rY, _ := b.push(10)
	var r3 entry

	var done bool
	go func() {
		b.Enqueue(1, func(uint16) error {
			r3, _ = b.push(1)
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
