package crow

import (
//	"fmt"
)

// note, CompareAndSwap(key, nil, new), key must exist
type ConcurrentMap interface {
        Clear()
        CompareAndDelete(key, old any) (deleted bool)
        CompareAndSwap(key, old, new any) (swapped bool)
        Delete(key any)
        Load(key any) (value any, ok bool)
        LoadAndDelete(key any) (value any, loaded bool)
        LoadOrStore(key, value any) (actual any, loaded bool)
        Range(f func(key, value any) bool)
        Store(key, value any)
        Swap(key, value any) (previous any, loaded bool)
}

type LockedMap struct {
	rb    Roundabout
	inner map[any]any
}


func (m *LockedMap) Load(key any) (value any, ok bool) {
	if m == nil {
		return nil, false
	}

	m.rb.SpinReadAll(func(epoch uint16, flags uint16) error {
		value, ok = m.inner[key]
		return nil
	})
	return
}

func (m *LockedMap) Store(key, value any) {
	m.rb.SpinLockAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.inner = make(map[any]any, 8)
		}
		m.inner[key] = value
		return nil
	})

}

func (m *LockedMap) Swap(key, value any) (previous any, loaded bool) {
	return nil, false
}

func (m *LockedMap) CompareAndDelete(key, old any) (deleted bool) {
	return false
}

func (m *LockedMap) CompareAndSwap(key, old, new any) (swapped bool) {
	return false
}

func (m *LockedMap) Delete(key any) {
}


func (m *LockedMap) LoadAndDelete(key any) (value any, loaded bool) {
	return nil, false

}

func (m *LockedMap) LoadOrStore(key, value any) (actual any, loaded bool) {
	return nil, false
}

func (m *LockedMap) Range(f func(key, value any) bool) {

}

func (m *LockedMap) Clear() {
}

