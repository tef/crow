# Roundabouts

A roundabout is like a write-ahead-log: There's a shared log, threads write their operations to it, and use it to ensure conflicting operations don't happen at the same time. As a result, the log can be used for big locks, fine grained locks, reader-writer locks, as well as coordinating things like resizing or snapshots. It's technically a phase concurrent data structure, if that helps.

The name stems from the traffic intersection. A thread yields to other threads already on the roundabout, chooses a lane to occupy, and makes progress if no-one's ahead of them, before exiting.


It works something like this:

- There is a global log of entries, numbered in order.
- Threads that want to do some concurrent action, publish an entry to the log.
- Threads then check all active predecessors in the log, to see if they would interfere.
- After waiting out the conflicting threads, it is safe to do work, as anyone conflicting with us has either completed, or is waiting for us to complete.
- Now the work is done, we mark this log entry as inactive, and any other threads waiting on us can make progress.

Entries in the log have a `kind`, and a `lane` to explain what sort of operation they are:

- `Shared` marks the entry as a read-only like operation, one that doesn't require ordering, or exclusivity.
- `Order` marks the entry as being a write operation, but one that only conflicts with itself, not `Shared`.
- `Lock` marks the entry as being an exclusive write operation, one that conflicts with `Order` and `Shared`.
- Entries can be marked as affecting all lanes, or a specific numbered lanes, to allow for finer-grained locks.

This allows a roundabout to offer something a little bit like locking, in several different flavours:

- Like a single, big lock, `LockRing`
	- The mutator threads spin until all predecessors are complete
	- Succesor threads spin when encountering a big lock in the log
    - Unlike a normal lock, threads establish priority in who gets to go next

- Like a reader, writer lock, `ShareRing`
    - If a ShareRing encounters another ShareRing/ShareLane, it continues on
	- Only Locks force mutual exclusion of Share, Order's are unaffected

- Like a fine grained lock, `LockLane`
	- Each thread inserts a 32bit lane, and spins if there's a matching lane
    - If they encounter a `Lock`, they wait for it too
    - There's also `ShareLane` which only conflict with `Lock`s of the same key

- Like Read-Copy-Update, `Fence`, `Phase`
    - A fence can be used to notify all future writers an operation is in progress, via a uint16 of flags
    - A fence can wait for all earlier writers to exit before starting work
    - Can be used to handle concurrent resizes or snapshots, without blocking writers

- Like Optimistic Locks `rb.Epoch()`, or Lock-Free-Reclamation `rb.Active(epoch)`
    - Checking the epoch before and after reads to check for changes
    - Can note down the epoch when a structure is retired
    - Can check if epoch has advanced, or all earlier writers have exited
    - Can be used to reclaim shared structures, or keep thread local free lists

Underneath, A roundabout is a fixed sized log that uses a bitfield to speed up scanning over old entries.

- There is a 32 element ringbuffer, and an int64 header including an epoch and a bitfield
- Threads announce work with a (epoch, kind, lane) entry on the log.
- Kind is Share/Order/Lock, on a particular lane, or all lanes
- Two `Share`/`Order`/`Lock` on different lanes will not wait for each other
- `Share` is unordered, `Order` is non-exclusive, but ordered , and `Lock` is exclusive and ordered.

It is important to note that it really isn't a lock. It's a log.  Each log entry represents a complete operation that a thread intends to carry out. In other words, each thread should only take up one entry in the log. Trying to allocate a Lock inside a Lock, for example, would stop the log from being emptied out, and potentially forcing
a deadlock.

## Implementation overview: Fixed size roundabout.

Underneath, It's really just a fancy ring buffer.

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

## Larger Roundabouts

The problem with having a larger log is that for the most part, threads will spend longer amounts of time
scanning the log to look for conflicts. On the other hand, having a shorter log leads threads contending to
get on the log, with the potential for some threads stalling hard.

Still, It's possible and useful to have a roundabout of more than 32 entries. One easy way is to use a big lock around the buffer, which ensures that the bitfield and epoch are in sync, but it might not be for everyone. Two alternatives are a roundabout of buckets, and a linked list of roundabouts.


## Roundabout of Buckets

The logic might go something like this:

- partition ring buffer into smaller buckets
- header keeps in-use/free information for each bucket, rather than per cell
- each bucket has it's own bitmap, tracking individual active/dead status
- high bits of epoch match to a bucket, low bits match to a cell within each bucket

There is a potential race that arises from the bucket bitmap and the header being in different structures:

This way lies false positvies or false negatives.

We have to carefully order the operations:

To allocate
- increment epoch, if it's in the same bucket, then we insert out item, we're done
- if it's in a new bucket, we first check to see if the bucket header is reader,  we mark bucket as in use
and then update our item

To scan
- we look at the bucket bitmap in the header, and pull in the bucket headers when scanning
- although a new bucket will be marked all "in-use", we will only be checking our predecessors

To remove
- we update our bitmap, and if it's now all 0's for this bucket, we update the header

If we, for example, tried updating the bucket bitmap after allocating, we would see a race, a later thread arriving into the bucket might assume an earlier entry has completed, rather than not yet been marked as in-use. If, after seeing that the bucket is empty, try to reset the bucket bitmap, we also introduce races.

If we reset the bucket bitmap to 'in use' after 'free' before we update the header, another process might accidentally see it as 'in-use'. If we mark the bucket as clear in the header first, another process might see it as 'free' before 'in-use' and skip over slots.

This is why we choose to update the bucket bitmap in the allocation process, before updating the header. The bucket will have already been marked as clear, so no-one will be looking at the bucket bitmap when we change it. Give or take a thread that stalled so hard that the entire block got reallocated, but that's a different problem and we solve that with epochs.


## Linked lists of roundabouts

One potential solution to this is something like a linked list of roundabouts

- There's the currently active roundabout, which loops around as usual
- If the roundabout is full, a new roundabout is allocated, and threads fill up the buffer as before, but
  do not scan predecessors, or don't have to, and spin on the original roundabout waiting to for it to empty out
- Once the current roundabout has been drained, the replacement is made active, and any thread waiting on it will see the change, and begin scanning, if they haven't already
- Any free roundabout can be put on a list for reuse, using the epoch

## Other implementation notes

For some structures, lanes aren't very useful. It might be better to use that space for other things. For example, we could keep a count of active readers inside a `Share` entry, along with a bit to say it is draining. Locks scan the log as normal, but mark all predecessors to drain. We also use a flag bit to indicate if the last log entry contains a count.

A roundabout can avoid scanning the log if it knows it's going to have to wait on every process, and simply spin on the bitfield instead. It's worth noting that if you only had Locks, you wouldn't need a ring buffer. If you didn't have lanes, and only used Orders and Share, you wouldn't need a buffer either.



