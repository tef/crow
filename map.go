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

// A Big Locked Struct

type LockedMap struct {
	rb    Roundabout
	inner map[any]any
}

func (m *LockedMap) Load(key any) (value any, ok bool) {
	if m == nil {
		return nil, false
	}

	m.rb.ShareRing(func(epoch uint16, flags uint16) error {
		value, ok = m.inner[key]
		return nil
	})
	return
}

func (m *LockedMap) Store(key, value any) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.inner = make(map[any]any, 8)
		}
		m.inner[key] = value
		return nil
	})

}

func (m *LockedMap) Swap(key, value any) (previous any, loaded bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.inner = make(map[any]any, 8)
		}
		previous, loaded = m.inner[key]
		if !loaded {
			previous = value
		}
		m.inner[key] = value
		return nil
	})
	return
}

func (m *LockedMap) CompareAndDelete(key, old any) (deleted bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
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
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
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
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		delete(m.inner, key)
		return nil
	})
}

func (m *LockedMap) LoadAndDelete(key any) (value any, loaded bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
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
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
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
	m.rb.OrderRing(func(epoch uint16, flags uint16) error {
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
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		m.inner = make(map[any]any, 8)
		return nil
	})
}

// Locked with Update

type BoxedMap struct {
	rb    Roundabout
	inner map[any]*atomic.Value
}

func (m *BoxedMap) Load(key any) (value any, ok bool) {
	if m == nil {
		return nil, false
	}

	m.rb.ShareRing(func(epoch uint16, flags uint16) error {
		v, loaded := m.inner[key]
		if loaded && v != nil {
			value = v.Load()
			ok = loaded
		}
		return nil
	})
	return
}

func (m *BoxedMap) init() {
	m.inner = make(map[any]*atomic.Value, 8)
}

func (m *BoxedMap) Store(key, value any) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.init()
		}
		v := new(atomic.Value)
		v.Store(value)
		m.inner[key] = v
		return nil
	})

}

func (m *BoxedMap) Swap(key, value any) (previous any, loaded bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			m.init()
		}

		v, loaded := m.inner[key]
		if loaded && v != nil {
			previous = v.Load()
			v.Store(value)
		} else {
			v := new(atomic.Value)
			v.Store(value)
			m.inner[key] = v
			previous = value
		}

		return nil
	})
	return
}

func (m *BoxedMap) CompareAndDelete(key, old any) (deleted bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		v, ok := m.inner[key]
		if ok && v == nil {
			value := v.Load()
			if value == old {
				delete(m.inner, key)
				deleted = true
			}
		}

		return nil
	})
	return
}

func (m *BoxedMap) CompareAndSwap(key, old any, newv any) (swapped bool) {
	m.rb.OrderRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		v, ok := m.inner[key]
		if ok && v == nil {
			value := v.Load()
			if value == old {
				m.inner[key].Store(newv)
				swapped = true
			}
		}

		return nil
	})
	return
}

func (m *BoxedMap) Delete(key any) {
	// if delete put tombstone in atomic value, this
	// could be shared write
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		delete(m.inner, key)
		return nil
	})
}

func (m *BoxedMap) LoadAndDelete(key any) (value any, loaded bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		v, ok := m.inner[key]
		if ok && v != nil {
			value = v.Load()
			loaded = ok
		}

		delete(m.inner, key)
		return nil
	})
	return

}

func (m *BoxedMap) LoadOrStore(key, value any) (actual any, loaded bool) {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		if m.inner == nil {
			return nil
		}
		v, ok := m.inner[key]
		if ok {
			loaded = false
			actual = value

			m.inner[key].Store(value)
		} else {
			loaded = true
			if v != nil {
				actual = v.Load()
			}
		}
		return nil
	})
	return
}

func (m *BoxedMap) Range(f func(key, value any) bool) {
	// inserts/deletes or anything triggering resize should be fine
	// and other reads should be fine, and the values
	// inside are atomic
	m.rb.ShareRing(func(epoch uint16, flags uint16) error {
		if len(m.inner) == 0 {
			return nil
		}
		for k, v := range m.inner {
			var a any
			if v != nil {
				a = v.Load()
			}
			if !f(k, a) {
				break
			}
		}
		return nil
	})

}

func (m *BoxedMap) Clear() {
	m.rb.LockRing(func(epoch uint16, flags uint16) error {
		m.init()
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
		if miss, ShareRing() on write
	insert
		load from read, check for expunged record, if so,
		insert it into write, and unexpunge it
		else create new and insert into write
	delete
		if in read, atomically update value
		if not in read, WriteRing to check write
			and delete value with nil - can't delete unless we're sure it's not in read
	update
		if in read, atomically update value
		if in write, WriteRing ..

	on several misses
		move write into read, maybe deleting old values
	on insert
		copy read into write, skipping deleted record, marking them as dead

	could have a map of deleted[key] in the write bit

*/
