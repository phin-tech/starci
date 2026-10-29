---
status: accepted
---

# A store of pointers, and edges that are written down

A step is given repository paths and nothing else, so what one step builds
cannot reach the steps after it, even on one machine, and an engine that
resumes runs keeps every result it has handed out. So the store, which held
only passing results, also holds what steps print and the files they keep,
by the hash of their content, and a result carries pointers to them rather
than the bytes. A run's record stays small whatever its steps produce, a
pointer is the same on every evaluation of a script, and a step given a
pointer is keyed on it: a build that makes the same binary again lets the
steps after it be reused.

A step is given another step's files only when the script names them. Nothing
arrives because of `needs`: what a step sees is what it was handed, as with
its inputs, and the edge is in the script for a reader and in the plan as a
need.
A basic script names them before anything has run, with a placeholder that
stands for a step's file and implies the need.

There is always a store: a directory on the machine, or a shared bucket. Only
starci writes to it, never a step: an image step never sees its credentials,
and a bare step none of the machine's environment but its PATH.

Built so far: artifacts, kept and handed on through a directory on the
machine or a bucket. What a step prints is still in its result, up to 64 KB,
until something needs it out of there.

## Considered options

- Everything from `needs`, at the same paths: less to write, but a step's key
  would change whenever something upstream kept one more file, and its
  workspace would hold files nothing asked for.
- Tarballs of directories: their timestamps change the hash every build. A
  directory is a list of its files, each a blob.

## Later, and how

Designed but not built, because nothing needs them yet:

- Signed results, when untrusted code can write to a store: an HMAC with a
  key only trusted runs hold, and a pass reused only if it checks.
- Scopes, when branches share a store: each writes under a prefix of its
  own and reads its own, then its base branch's, as the GitHub Actions cache
  does.
- Two roles with backends of their own: records (results, passes), small and
  written only if absent, in a directory, a bucket, Redis, or a CI tool's
  cache; blobs in a directory or a bucket. Redis is for records only, and
  only if it will not evict them.
