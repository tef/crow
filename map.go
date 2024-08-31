package crow

import (
	"sync/atomic"
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

	m.rb.ReadAll(func(epoch uint16, flags uint16) error {
		value, ok = m.inner[key]
		return nil
	})
	return
}

func (m *LockedMap) Store(key, value any) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.inner = make(map[any]any, 8)
		}
		m.inner[key] = value
		return nil
	})

}

func (m *LockedMap) Swap(key, value any) (previous any, loaded bool) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.inner = make(map[any]any, 8)
		} else {
			previous, loaded = m.inner[key]
		}
		m.inner[key] = value
		return nil
	})
	return
}

func (m *LockedMap) CompareAndDelete(key, old any) (deleted bool) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		v, ok := m.inner[key]
		if ok && v == old {
			delete(m.inner, key)
			deleted = true
		}

		return nil
	})
	return
}

func (m *LockedMap) CompareAndSwap(key, old, new any) (swapped bool) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		v, ok := m.inner[key]
		if ok && v == old {
			m.inner[key] = new
			swapped = true
		}

		return nil
	})
	return
}

func (m *LockedMap) Delete(key any) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		delete(m.inner, key)
		return nil
	})
}

func (m *LockedMap) LoadAndDelete(key any) (value any, loaded bool) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		value, loaded = m.inner[key]
		delete(m.inner, key)
		return nil
	})
	return

}

func (m *LockedMap) LoadOrStore(key, value any) (actual any, loaded bool) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		actual, loaded = m.inner[key]
		if !loaded {
			m.inner[key] = value
		}
		return nil
	})
	return
}

func (m *LockedMap) Range(f func(key, value any) bool) {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		if len(m.inner) == 0 {
			return nil
		}
		for k, v := range m.inner {
			if !f(k, v) {
				break
			}
		}
		return nil
	})

}

func (m *LockedMap) Clear() {
	m.rb.ExWriteAll(func(epoch uint16, flags uint16) error {
		m.inner = make(map[any]any, 8)
		return nil
	})
}

// sync.Map style, with an unlocked read only copy

type map_entry struct {
	value atomic.Value
}

type ReadWriteMap struct {
	rb    Roundabout
	read  atomic.Pointer[map[any]*map_entry]
	write map[any]*map_entry
}

/*
	read
		load from read, check for dead or nil entry
		if miss, ReadAll() on write
	insert
		load from read, check for expunged record, if so,
		insert it into write, and unexpunge it
		else create new and insert into write
	delete
		if in read, atomically update value
		if not in read, WriteAll to check write
			and delete value with nil - can't delete unless we're sure it's not in read
	update
		if in read, atomically update value
		if in write, WriteAll ..

	on several misses
		move write into read, maybe deleting old values
	on insert
		copy read into write, skipping deleted record, marking them as dead

	could have a map of deleted[key] in the write bit

*/
