# Roundabouts

A roundabout is like a write-ahead-log. Threads publish what they want to work to the log, scan the predecessors for conflicts, and then clear their entry when done. This allows a roundabout to be used for mutual exclusion, as well as coordination between threads.

It works something like this:

- There is a global log of cells, and an epoch (or log number)
- Threads that want to do some concurrent action, publish an entry to the log
- Threads then check all active predecessors in the log, to see if they would interfere
- After waiting out the conflicting threads, it is safe to do work
- Now the work is done, we mark this log entry as inactive

The implementation here is slightly different:

- There is a 32 element ringbuffer, and an int64 header including an epoch and a bitfield
- Threads announce work with a (kind, lane) pair
- Kind is Share/Order/Lock, on a paritcular lane, or all lanes
- Share is unordered, Order can overlap with Share, but Lock precludes all
- Two Share/Order/Lock on different lanes will not wait for each other
- Share never wait for each other, Order, Lock do

A roundabout can be used for big locks, fine grained locks, reader-writer locks, as well as coordinating things like resizing or snapshots.  The name stems from the traffic intersection. A thread yields to other threads already on the roundabout, chooses a lane to occupy, and makes progress if no-one's ahead of them, before exiting. 

Here's what it looks like:

```
r := Roundabout{}

r.LockRing(func(epoch uint16, flags uint16) error{
    // will only run after all older threads exit
    // will stall all newer threads from running

    ...
})


var lane uint32 = 12234
r.LockLane(lane, func(epoch uint16, flags uint16) error {
    // this callback will never be invoked while other callbacks
    // are running with the same lane

    ...
})
r.ShareLane(lane, func(epoch uint16, flags uint16) error {
    // wait for Lockrs, but don't wait for Reads
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

A roundabout has a tiny ring buffer underneath to store log entries. Each entry has
a `lane`, and a `kind` to let other threads reading the log if there's a conflict.

This allows a roundabout to offer something a little bit like locking, in several different flavours:

- Like a single, big lock, `r.LockRing(...)`
	- The mutator threads spin until all predecessors are complete
	- Succesor threads spin when encountering a big lock in the log
    - Unlike a normal lock, threads establish priority in who gets to go next

- Like a reader, writer lock, `r.ShareRing(...)`
    - If a ShareRing encounters another ShareRing/ShareLane, it continues on
	- Only Locks force mutual exclusion of Share, Order's are unaffected

- Like a fine grained lock, `r.LockLane(lane, ...)`
	- Each thread inserts a 32bit lane, and spins if there's a matching lane
    - If they encounter a big lock, they wait for it too
    - There's also `r.ShareLane(lane, ...)` which only conflict with writes of the same key

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
- There's items of (epoch, kind, lane32)
	- The epoch lets us know if an item comes before or after us
	- Kind indicates what sort of entry (ShareLane, LockLane, etc)
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
	- Replace item with (epoch+width, free_kind, 0)
	- This lets later writers skip the entry, or spin until it's allocated
	- CAS in a new header with the bitfield updated

## Magic Roundabout 

It's possible to have a roundabout of more than 32 entries, You could chain
them up in a linked list, you could have a larger bitfield and use the epoch
to lock updates, but I think there's a third way involving partitioning the
ring into smaller buckets, and keeping summary information in the header

The logic might go something like this:

- partition ring buffer into smaller buckets
- header keeps in-use/free information for each bucket, rather than per cell
- each bucket has it's own bitmap, tracking individual active/dead status 
- high bits of epoch match to a bucket, low bits match to a cell within each bucket

To allocate
- increment epoch, if it's in the same bucket, then we insert out item, we're done
- if it's in a new bucket, we first check to see if the bucket is free, we mark bucket as in use
and then update our item

To scan
- we look at the bucket bitmap in the header, and pull in the bucket headers when scanning

To remove
- we update our bitmap, and if it's now all 0's for this bucket, we reset the bitmap, 
- and then mark it as free in the header

To ensure there's no race between updating the header and a mark being in the bitmap for an active process,
we need to be careful, if we reset the bucket bitmap to 'in use' after 'free' before we update the header, another process might accidentally see it as 'in-use'. if we mark the bucket as clear in the header first, another process might see it as 'free' before 'in-use' and skip over slots. this way lies false positvies or false negatives.

Instead, we place the burden on the allocating thread, not the deallocating thread
- when a new item is the first item in a bucket, we check that it is clear in the header, 
- then we check the bucket bitmap to say "in use" for all, then we can allocate
- any new process that arrives will see the header as being clear, and skip it, or it will see a fully in use bucket, and skip over the later elements as not being < epoch
- if two processes race, it's stil safe. one will flip the bits first, and the other might allocate first, but neither is locked.

# Linked lists of roundabouts

The problem with having a larger log is that for the most part, threads will spend longer amounts of time
scanning the log to look for conflicts. On the other hand, having a shorter log leads threads contending to
get on the log, with the potential for some threads stalling hard.

One potential solution to this is something like a linked list of roundabouts

- There's the currently active roundabout, which loops around as usual
- If the roundabout is full, a new roundabout is allocated, and threads fill up the buffer as before, but
  do not scan predecessors, or don't have to, and spin on the original roundabout waiting to for it to empty out
- Once the current roundabout has been drained, the replacement is made active, and any thread waiting on it will see the change, and begin scanning, if they haven't already
- Any free roundabout can be put on a list for reuse, using the epoch

