# Concurrent Readers, Ordered Writers

A work in progress.

## Roundabouts

A roundabout is like a write-ahead-log. Threads publish what they want to work to the log, scan the predecessors for conflicts, and then clear their entry when done. This allows a roundabout to be used for mutual exclusion, as well as coordination between threads.

A roundabout can be used for big locks, fine grained locks, reader-writer locks, as well as coordinating things like resizing or snapshots.

Here's what it looks like:
r := Roundabout{}

var key uint32 = 12234
r.SpinLock(key, func(epoch uint16, flags uint16) error {
    // this callback will never be invoked while other callbacks
    // are running with the same key

    ...
})

r.SpinLockAll(func(epoch uint16, flags uint16) error{
    // will only run after all older threads exit
    // will stall all newer threads from running

    ...
})

r.SpinRead(key, func(epoch uint16, flags uint16) error {
    // this callback will never be invoked while other callbacks
    // are running with the same key, unless those are also SpinReads
    ...
})

flags := b1001_0101
r.Fence(flags, func(epoch uint16, flags uint16) error {
    // this updates the flags for all subsequent threads
    // this callback will not run until all previous threads complete
    // and after it exits, the flags are changed back
}

r.Phase(flags, func(epoch uint16, flags uint16) error {
    // this updates the flags for all subsequent threads
    // this callback will not run until all previous threads complete
    // and after it exits, the flags are changed back
   ....
}, func(old uint16, new uint16) {
    // this callback runs after the flags have been reset
    // and is passed the start and end epochs for the operation

   ....
})

```

Despite being a log, a roundabout sits between "fine grained locks" and "one big lock". A Roundabout to be used in a number of different ways:

- Like a fine grained lock, `r.SpinLock(key, ...)`
	- Each thread inserts a 32bit key, and spins if there's a match
- Like a single, big lock, `r.SpinLockAll(...)`
	- The mutator threads spin until all predecessors are complete
	- Succesor threads spin when encountering a big lock in the log
- Like a reader, writer lock, `r.SpinRead(key, ...)`
	- Readers create conflict free log items
	- Only writers force mutual exclusion
- Like an optimistic read lock, `r.Epoch()`
	- Checking the epoch before and after reads to check for changes
- Like an epoch based reclaimer, `r.Fence()`, `r.Phase()`
    - The buffer has an `uint16` of flags for user use. This gets passed into all SpinLocks
	- Changing the header affects all subsequent writers, but not previous ones.
	- Unlike `SpinLock`, doesn't block new writers
	- Waits for all predecessor log entries to complete, runs callback
	- Can be used to handle concurrent resizes or snapshots


Underneath, it's comprised of a ring buffer of work items, using a bitfield instead of a count to manage freeing items. This allows us to do some not very ring buffer things like removing arbitrary items from the list, but it also limits us to having a quite small ringbuffer.

- There's a header of (epoch, flags, bitfield32)
	- The epoch is the next free slot
	- The bitfield tracks which items are allocated in the ring buffer
	- The flags are passed on to mutator threads allocating
- There's items of (epoch, state, key32)
	- The epoch lets us know if an item comes before or after us
	- State lets us know if it's a special key
	- Key lets us find conflicting items
- There's only 32 slots in the ring buffer
    - That's ok though, 32 is a pretty big number in terms of active CPUs


The operations are pretty much what you'd do for a ring buffer, but
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
