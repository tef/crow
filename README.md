# Concurrent Readers, Ordered Writers

A work in progress.

## Roundabouts

A roundabout is like a write-ahead-log. Threads publish what they want to work to the log, scan the predecessors for conflicts, and then clear their entry when done. This allows a roundabout to be used for mutual exclusion.

Here's what it looks like:

```
// an empty struct is always a valid
r := crow.Roundabout{}

var key uint32 = 12234
r.Spinlock(key, func(flags uint16) {
    // this callback will never be invoked while other callbacks
    // are running with the same key
    ...
})
```

This has some advantages and disadvantages, or tradeoffs.

Despite being a log, a roundabout sits between "fine grained locks" and "one big locks", an alternative to RWLocks:

- Like a Big Lock, the roundabout can offer exclusive access
- Like a RWLock, there is a bound on the number of users (here, 32)
- Like Fine Grained Locking, multiple writers can be active
- Unlike fine grained locking, there isn't any per-item overhead

Underneath, it's comprised of a ring buffer of work items, using a bitfield instead of a count to manage freeing items. This allows us to do some not very ring buffer things like removing arbitrary items from the list, but it also limits us to having a quite small ringbuffer.

A roundabout supports several types of work item:

- One that tells other threads to spin
- One that tells other threads to abort
- One that tells every thread to spin
- One that tells every thread to abort

A roundabout also, in true unix style, flags. Inside the ring buffer, there's a uint16 field, which gets passed to each new thread. This allows a roundabout to advise new threads that an action is in progress.

```
flags := 0b1101
rb.Signal(flags, func() error{
    // flags will be set for all new threads
    // and the callback will run when no old threads
    // are active
    ...
})

rb.Phase(flags, func() error{
    // flags will be set for all new threads
    // and the callback will run when no old threads
    // are active
    ...
}, func (start, end uint16) error{
    // run once flags are cleared

})
```


