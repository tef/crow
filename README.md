# Concurrent Readers, Ordered Writers

A work in progress. This library may eventually contain examples
of concurrent data structures, but for now it only contains
the mechanims to build them.

The big idea is using a tiny in-memory write ahead log to 
order operations, and resolve conflicts. 

## Roundabouts

A roundabout is like a write-ahead-log. Threads publish what they want to work to the log, scan the predecessors for conflicts, and then clear their entry when done. This allows a roundabout to be used for mutual exclusion, as well as coordination between threads.

A roundabout can be used for big locks, fine grained locks, reader-writer locks, as well as coordinating things like resizing or snapshots.  The name stems from the traffic intersection. A thread yields to other threads already on the roundabout, chooses a lane to occupy, and makes progress if no-one's ahead of them, before exiting. 

Here's what it looks like:

```
r := Roundabout{}

var lane uint32 = 12234
r.SpinLock(lane, func(epoch uint16, flags uint16) error {
    // this callback will never be invoked while other callbacks
    // are running with the same lane

    ...
})

r.SpinLockAll(func(epoch uint16, flags uint16) error{
    // will only run after all older threads exit
    // will stall all newer threads from running

    ...
})

r.SpinRead(lane, func(epoch uint16, flags uint16) error {
    // this callback will never be invoked while other callbacks
    // are running with the same lane, unless those are also SpinReads
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

The lane passed to a lock gets written into the log buffer, and the other threads compare
it against their own lane, and type, to know if to spin or if it is safe to continue on
while the other thread is active. This allows a roundabout to offer something a little
bit like locking, in several different flavours:

- Like a single, big lock, `r.SpinLockAll(...)`
	- The mutator threads spin until all predecessors are complete
	- Succesor threads spin when encountering a big lock in the log
    - Unlike a normal lock, threads establish priority in who gets to go next

- Like a reader, writer lock, `r.SpinReadAll(...)`
    - If a SpinRead encounters another SpinRead, it continues on
	- Only writers force mutual exclusion

- Like a fine grained lock, `r.SpinLock(lane, ...)`
	- Each thread inserts a 32bit lane, and spins if there's a matching lane
    - If they encounter a big lock, they wait for it too
    - There's also `r.SpinRead(lane, ...)` which only conflict with writes of the same key

- Like Read-Copy-Update, `r.Fence()`, `r.Phase()`
    - A fence can be used to notify all future writers an operation is in progress, via a uint16 of flags
    - A fence can wait for all earlier writers to exit before starting work
    - Can be used to handle concurrent resizes or snapshots, without blocking writers

- Like Optimistic Locks `r.Epoch()`, or Lock-Free-Reclamation `r.Active()`
    - Checking the epoch before and after reads to check for changes
    - Can note down the epoch when a structure is retired
    - Can check if epoch has advanced, or all earlier writers have exited
    - Can be used to reclaim shared structures, or keep thread local free lists

It is important to note that it really isn't a lock. It's a log.  Each log entry represents a complete operation that a thread intends to carry out. In other words, each thread should only take up one entry in the log. Trying to allocate a spinlock inside a spinlock, for example, would stop the log from being emptied out, and potentially forcing
a deadlock.

Aside: If an operation requires locking over two lanes, you'd need to allocate two entries
at the same time, and that has an awful lot of edge cases. It's easier to cram things into a uint32 and pass in a custom Conflict function to test them.

The big reason for this is that the log is represented by a fixed sized ring buffer, rather than a series of linked lists, which is a bit of a tradeoff, but it makes several operations much faster, primarily scanning over current log entries, as well as deleting log entries.

It's really just a fancy ring buffer.

- There's a header of (epoch, flags, bitfield32)
	- The epoch is the next free slot
	- The bitfield tracks which items are allocated in the ring buffer
	- The flags are passed on to mutator threads allocating
- There's items of (epoch, state, lane32)
	- The epoch lets us know if an item comes before or after us
	- State indicates what sort of entry (Read, SpinAll, Free)
	- Lane32 lets us find conflicting items
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
