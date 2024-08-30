package crow

import (
	//"fmt"
	"testing"
)

// reminder:
// t.Log(...) / t.Logf("%v", err)
// t.Error(...) Errorf,  mark fail and continue
// t.Fatal(...) FatalF,  mark fail, exit

func TestMap(t *testing.T) {

	m := &LockedMap{}

	m.Store("foo", "bar")
	out, ok := m.Load("foo")
	if !ok {
		t.Error("missing value")
	}

	s, ok := out.(string)
	if !ok {
		t.Error("bad value")
	} else if s != "bar" {
		t.Error("wrong value")
	}

	//t.Fatal()
	//t.Error()
	//t.Logf()
}

func BenchMap(b *testing.B) {
	// setup
	b.ResetTimer()
	for range b.N {

	}
	// or b.RunParallel(func(pb *testing.PB) {})
}
