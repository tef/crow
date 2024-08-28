# Concurrent Readers, Ordered Writers

A work in progress.

## Roundabouts

At the heart of this code is a tiny scheduler, based around a ring buffer. It's a concurrent data structure for building concurrent data structures:

- Threads write to this buffer, to announce work.
- They then scan ahead to look for conflicting work.
- They then remove themselves from the buffer when done.

It's designed as a replacement for 'one big lock' style concurrent data structures, The buffer itself is only 32 elements wide, so clients are expected to be hasty when using it.

Here's what it looks like:


```
var key uint32 = 12234

r := crow.Roundabout{}

// this callback will never be invoked while other callbacks
// are running with the same key

r.Enqueue(key, func(flags uint16) { 
     ...
})
```

The underlying structure itself is relatively novel, but it's a combination of textbook features. At the heart of it is a ring buffer that uses a (epoch, bitfield) header instead of (start, end) indices, and the buffer entries themselves are (epoch, key) pairs.

The epoch forms a generational index into the ring buffer, and the bitfield acts as a freelist for the buffer, removing the need for a 'end' value in the header.

It works a little like this: A process announces work by claiming the next free bit in the buffer, then inserting a (count, item) pair into the buffer. Using the bitfield to skip over dead entries, the process looks for any record with an earlier count, and checks for conflicts. Once complete, the (count, item) is replaced with (count+32, nil) and the bitfield is updated so that another process can reclaim the item.

This has some advantages and disadvantages

- It only allows for 32 active processes, and one stuck process can stall new processes from working.
- When compared to fine grained locks, it has only a few advantages. There's no need to resize or dynamically allocate the structure, and it's possible to do 'one big lock' style syncronization

There's a little bit of space left over in the int64s, so in true unix style, we added flags:

- There's a uint16 value passed to processes when running
- These flags are stored in the header
- Changing these flags updates all future processes
- It's possible to use this to advise processes of things like resizing or snapshots, and act accordingly.

