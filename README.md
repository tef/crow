# Concurrent Readers, Ordered Writers

A work in progress.

## Roundabouts

A roundabout is like a write-ahead-log. Threads publish what they want to work to the log, scan the predecessors for conflicts, and then clear their entry when done. This allows a roundabout to be used for mutual exclusion, as well as coordination between threads.

Here's what it looks like:

```
r := crow.Roundabout{}

var key uint32 = 12234
r.SpinLock(key, func(flags uint16) {
    // this callback will never be invoked while other callbacks
    // are running with the same key

    ...
})

r.SpinLockAll(func(flags uint16) {
    // will only run after all older threads exit
    // will stall all newer threads from running

    ...
})
```

This has some advantages and disadvantages.

Despite being a log, a roundabout sits between "fine grained locks" and "one big locks":

- Like a Big Lock, the roundabout can offer exclusive access.
- Like a RWLock, there is a bound on the number of users (here, 32)
- Like Fine Grained Locking, multiple writers can be active.
- Unlike fine grained locking, there isn't any per-item overhead.

Underneath, it's comprised of a ring buffer of work items, using a bitfield instead of a count to manage freeing items. This allows us to do some not very ring buffer things like removing arbitrary items from the list, but it also limits us to having a quite small ringbuffer.

In true unix style, a roundabout also has flags. Inside the ring buffer, there's a uint16 field, which gets passed to each new item on the buffer. This allows a roundabout to advise new threads that an action is in progress.

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

