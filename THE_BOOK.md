# DiceMe — The Complete Book
**file:///Users/shivam.garg/Documents/Redis%20Internals%20-%20Reading%20Materials.pdf**
**Everything needed to build a production-grade Redis from scratch, in Go, starting from the DiceMe you already have.**

Reference source: `~/Code/Learning/redis` @ commit `d22066d09` (version 8.9.241). Every `file:function` reference in this document is verified against that commit. Line numbers drift across versions; function names don't — search for the function, not the line.

> **A note on Redis versions.** The tree you have is a modern one. Some internals were rewritten after Redis 7: the per-db `dict` became `kvstore.c` (one dict per cluster slot), the `expires` dict became `ebuckets.c`/`estore.c` (a bucketed TTL index), and key+value merged into a single `kvobj` allocation. **You should build the classic model** — one hash table for keys, one for expires, exactly the shape your `core/store.go` already has — because it is simpler, it is what every paper and blog post describes, and the new code is an optimization of the same semantics. Where the modern code differs from what you build, this book says so explicitly.

---

## How to use this document

Chapters run 1 to 48 in order. Read chapter 1, then 2, then 3, to the end.

**The target is a replacement.** When you finish chapter 48, a client library, a monitoring exporter, and `redis-cli` should be unable to tell DiceMe from `redis-server` — that is the acceptance criterion the whole book is arranged around, and chapter 48 is where somebody else's test suite decides whether you met it. The one thing that stays out of reach is the C modules ABI (§48.4 explains why, and what it costs).

The nine parts:

| Part | Chapters | What you have when it's done |
|---|---|---|
| **I — Orientation** | 1–2 | The one architectural decision the whole project rests on |
| **II — The server core** | 3–7 | A server `redis-benchmark` can hammer without falling over, speaking RESP2 and RESP3 |
| **III — Keyspace, strings, expiry** | 8–13 | Strings, key management and all 16 databases, byte-identical to real Redis |
| **IV — Data structures and collections** | 14–19 | dict, listpack, skiplist, intset, quicklist, rax — and lists/hashes/sets/zsets on top |
| **V — Persistence and eviction** | 20–26 | Survives `kill -9`; loads and writes RDB files real Redis accepts; evicts sensibly when memory fills |
| **VI — Replication** | 27–30 | A replica that partial-resyncs after a dropped link |
| **VII — Transactions and cluster** | 31–36 | MULTI/WATCH, blocking, pub/sub, and a 6-node cluster that fails over |
| **VIII — Production** | 37–38 | Sentinel's design, the operational and observability surface |
| **IX — The rest of real Redis** | 39–48 | Scripting, streams, bitmaps/HLL/geo, Sentinel, ACLs and TLS — then the compatibility sweep that proves it |

Appendices A–I are lookup: glossary, command surface, source index, reading list, self-tests, bug catalogue, cookbook, answer key, per-chapter resources.

**Diff against real Redis constantly.** You have the real thing: `redis-server` on 6379, DiceMe on 7379, same commands to both, compare bytes. The oracle is one process away, which no other from-scratch project of this size gets. Build the diff harness in chapter 6 and never stop using it.


## Prerequisites check

You need, before starting:

- **Go**: goroutines, channels, `select`, interfaces, `sync.Mutex`, slices/maps at the "append may reallocate" level, testing.
- **Networking**: TCP sockets, non-blocking I/O.
- **Command line**: `redis-cli`, `redis-benchmark`, `kill -9`, `hexdump -C`, reading logs.

You do **not** need prior database-internals knowledge, replication theory, or consensus.

---

## The audit — what DiceMe is today

A code review of your repo as it stands. Every finding is fixed in a specific chapter — the "Fixed in" column — not now.

### What you have

| Piece | State |
|---|---|
| `main.go` | Flag parsing, signal channel, starts async server. |
| `server/sync_tcp.go` | Goroutine-free blocking server (one client at a time) + `readCommands`/`respond` shared by both servers. |
| `server/async_tcp.go` | kqueue event loop: accept + read events, 1 s expiry cron, engine-status state machine for shutdown. |
| `core/resp.go` | RESP2 decode (all five types) + encode. Tests in `core/resp_test.go`. |
| `core/cmd.go`, `core/eval.go` | `RedisCmd`, 18 commands: PING, SET(EX), GET, TTL, DEL, EXPIRE, BGREWRITEAOF, INCR, INFO, CLIENT, LATENCY, LRU, SLEEP, EXISTS, KEYS, TYPE, RENAME, PERSIST. |
| `core/object.go`, `core/typeencoding.go`, `core/type_string.go` | `Obj{TypeEncoding, LastAccessedAt, Value}`; type in high nibble, encoding in low nibble; string encodings INT / EMBSTR(≤44) / RAW. This is the real Redis scheme and it survives to the end. |
| `core/store.go` | `store map[string]*Obj` + `expires map[string]uint64` (ms timestamps) — the classic two-dict model. Put/Get/Del/Rename. Overwrite-drops-TTL is correct SET semantics. |
| `core/expire.go` | Lazy expiry on Get + active sampling: 20 keys from `expires`, repeat while >25% expired. The real algorithm's skeleton. |
| `core/eviction.go`, `core/evictionpool.go` | Three strategies; 24-bit LRU clock with wraparound-aware idle time; 16-slot eviction pool. The real algorithm's skeleton. |
| `core/aof.go` | AOF dump of the store as RESP `SET` commands. |
| `storm/set/main.go` | 5-connection SET load generator. |
| `monitoring/` | Prometheus + Grafana scaffolding — wired up in ch. 38 (§38.1). |

That is a legitimate skeleton of chapters 7 and 11, plus previews of 23 and 26. You are not starting from zero; you're starting from ~15%.

### The findings

Severity: 🔴 crashes or corrupts, 🟠 wrong behavior a client can observe, 🟡 wrong internals / will block you later.

| # | Where | Finding | Fixed in |
|---|---|---|---|
| 1 | 🔴 `core/resp.go:readSimpleString` (and `readInt64`, `readBulkString`) | Not incremental. `for ; data[pos] != '\r'; pos++` runs off the buffer end on partial input → index-out-of-range **panic**. `readBulkString` slices `data[pos : pos+len]` past the end → panic. TCP is a stream; half a command in one `read()` is normal, not exceptional (§3.3). | ch. 7 |
| 2 | 🔴 `server/sync_tcp.go:readCommands` | One `c.Read` into a 512-byte buffer, then decode. Any command >512 bytes, or a pipelined batch crossing the boundary, is truncated mid-frame → decode error or finding-1 panic → connection dropped. `redis-benchmark -P 10` kills DiceMe today. | ch. 7 |
| 3 | 🔴 `core/evictionpool.go:Push` (full-pool branch) | When the pool is full: compares raw `lastAccessedAt` (ignoring the 24-bit wraparound your own `getIdleTime` handles), **removes `pool[0]` — the best eviction candidate — to make room**, appends without re-sorting (pool order now corrupted), and never deletes the displaced item from `keyset` (leak → those keys can never enter the pool again). Real Redis inserts in ascending-idle order and drops the *smallest*-idle entry (`evict.c:evictionPoolPopulate`). | ch. 26 |
| 4 | 🔴 `core/aof.go:DumpAllAOF` | Opens with `O_APPEND` — every BGREWRITEAOF appends the **entire dataset again** (check `dice-master.aof`: duplicates already). No temp-file + rename (§21.2), no fsync, no TTL preservation, values containing spaces corrupt the file (`strings.Split(cmd, " ")`), and there is no load-on-boot at all — persistence is write-only. Also runs synchronously in the event loop despite the BG name. | ch. 23 |
| 5 | 🟠 `core/eval.go:EvalAndRespond` default branch | Unknown command falls through to `evalPING` → `FOOBAR` returns `+PONG`. Must be `-ERR unknown command 'foobar'`. Silent-wrong is the worst failure mode a server can have. | ch. 7 |
| 6 | 🟠 `server/async_tcp.go` main loop | `syscall.Kevent(kqueueFD, nil, events, nil)` — nil timespec blocks **forever**. The expiry cron runs only when client traffic happens to wake the loop; on an idle server, keys never actively expire. Real `ae.c` computes the poll timeout from the nearest timer (§3.5). | ch. 7 |
| 7 | 🟠 `core/eval.go:evalSET` / `evalEXPIRE` | `EX 0` silently ignored (`exDurationMs > 0` guard) — real Redis errors `invalid expire time`. `EXPIRE` with 0/negative TTL must **delete** the key and report 1, not set a past timestamp that leaves a phantom key (§7.2). `TTL` rounds down (real: rounds to nearest — 900 ms left is `1`, not `0`). | ch. 11 |
| 8 | 🟠 `core/store.go:Put` | Evicts when `len(store) >= config.KeysLimit` — a **key count** of 100, not a memory budget. Your own storm generator trips constant eviction. Becomes `maxmemory` byte accounting in ch. 26 (§23.2). | ch. 26 |
| 9 | 🟠 `server/async_tcp.go:WaitForSignal` | `for atomic.LoadInt32(&eStatus) == BUSY {}` — busy-spins a full core while waiting. Also `switch eStatus` later reads the variable non-atomically. Dies with the architecture change in ch. 7 (§14.4). | ch. 7 |
| 10 | 🟡 `core/eval.go:evalINCR` | Parse error ignored (`i, _ := strconv.ParseInt`), no int64 overflow check (real Redis: `increment or decrement would overflow`), and error text doesn't match Redis's `ERR value is not an integer or out of range` — your diff harness will flag every mismatch like this. | ch. 11 |
| 11 | 🟡 `core/expire.go:DeleteExpiredKeys` | The 25%-loop has no **time budget**. After mass expiry (imagine 1M keys with the same deadline), the loop runs unbounded inside the cron and blocks all clients. Real Redis caps the cycle at a time slice (`expire.c:activeExpireCycle`, §7.3). | ch. 11 |
| 12 | 🟡 `core/eviction.go` | `evictAllkeysLRU` evicts 40% of KeysLimit in one burst (real Redis frees just enough to get under `maxmemory`); pool is populated with only 5 samples once per burst; `volatile-*` policies (sample from `expires`, not `store`) don't exist yet. | ch. 26 |
| 13 | 🟡 `core/store.go:Rename` | Calls `Put(newKey)`, which can trigger eviction mid-rename — with terrible luck the evictee is the very object being renamed. Rename must be a plain swap, never a capacity-checked insert. Also: first-ever `Del` before any `Put` panics on the nil `KeyspaceStat[0]` map. | ch. 11 |
| 14 | 🟡 `core/eval.go:evalKEYS` | Uses `path.Match`: `/` is special, `[` unclosed returns an error (your `ERR invalid pattern`), neither matches Redis's `stringmatchlen` glob (which treats malformed patterns literally and errors on nothing). Write your own matcher in ch. 11 — it's 40 lines. | ch. 11 |
| 15 | 🟡 `main.go` | `wg.Add(2)` but `RunAsyncTCPServer`'s goroutine never calls `Done`; the process only exits because the signal handler calls `os.Exit`. Clean shutdown (finish in-flight command → flush AOF → close listeners) is a ch. 7 requirement. | ch. 7 |
| 16 | 🟡 `respond()` → `core/comm.go` | Replies written straight to the fd with a single `syscall.Write` — partial writes silently drop bytes, and there is no output buffer, so one slow client can block the loop. No `TCP_NODELAY` on accepted fds either (raw-syscall path — Nagle adds ~40 ms to small replies). | ch. 7 |
| 17 | 🟡 `core/resp.go:readLength` | Can't represent `-1`, so `$-1\r\n` (null bulk) mis-parses as an empty string with a wrong delta — matters the moment you *decode* replies (replication, ch. 30, speaks RESP back at you). | ch. 7 |
| 18 | 🟡 `core/eval.go:evalGET` | Dead second `hasExpired` check — `Get` already deleted the key. Harmless; symptomatic of expiry logic living in two places. One choke point: `expireIfNeeded` (§7.2). | ch. 11 |

Two things in the repo are **more right than they look**: the two-nibble `TypeEncoding` scheme is exactly `robj`'s `type`/`encoding` split and survives to the end of the project, and `getIdleTime`'s wraparound handling matches `evict.c:estimateObjectIdleTime`. Keep both.

---

## The map

Forty-eight chapters, in order. **B** marks a build chapter, **S** a source-reading tour.

```
PART I    ORIENTATION
   1      What Redis actually is                            2 h
   2      Threads in a "single-threaded" server             2 h   ← decides the architecture

PART II   THE SERVER CORE
   3      The event loop                                    5 h
   4      RESP2 and RESP3: the wire protocol                4 h
   5   S  Reading the source: the server core               7 h
   6      Project setup and conventions                     5 h
   7   B  Build: the server core                           31 h

PART III  KEYSPACE, STRINGS, AND EXPIRY
   8      The object model and the keyspace                 2 h
   9      Expiration                                        2 h
  10   S  Reading the source: keyspace and strings          3 h
  11   B  Build: keyspace, strings, expiry                 41 h   ← first milestone
  12      The big picture                                   2 h
  13      Request lifecycles, traced                        2 h

PART IV   DATA STRUCTURES AND COLLECTIONS
  14      Memory                                            3 h
  15      The data structures you will build               12 h
  16   S  Reading the source: the dict                      2 h
  17   B  Build: the data-structure libraries              55 h
  18   S  Reading the source: the collections             2.5 h
  19   B  Build: the collections                           48 h   ← genuinely useful

PART V    PERSISTENCE AND EVICTION
  20      Persistence: RDB and AOF                          4 h
  21      Crash safety: the filesystem contract             2 h
  22   S  Reading the source: RDB and AOF                   4 h
  23   B  Build: persistence                               53 h   ← no fork in Go
  24      Eviction                                          2 h
  25   S  Reading the source: eviction                    1.5 h
  26   B  Build: maxmemory and eviction                    23 h

PART VI   REPLICATION
  27      Replication                                       5 h
  28      Debugging a database                              2 h
  29   S  Reading the source: replication                   3 h
  30   B  Build: replication                               55 h   ← the wall

PART VII  TRANSACTIONS AND CLUSTER
  31      Transactions, blocking, pub/sub                   3 h
  32   S  Reading the source: MULTI, blocking, pub/sub    1.5 h
  33   B  Build: transactions, blocking, pub/sub           27 h
  34      Cluster                                           4 h
  35   S  Reading the source: cluster and Sentinel        2.5 h
  36   B  Build: cluster                                   59 h   ← the second wall

PART VIII PRODUCTION
  37      Sentinel                                          2 h
  38      Production-grade concerns                        17 h

PART IX   THE REST OF REAL REDIS
  39      Scripting: Lua and Functions                      3 h
  40   B  Build: scripting                                 30 h
  41      Streams                                           3 h
  42   B  Build: streams                                   35 h
  43      Bitmaps, HyperLogLog, and geospatial              3 h
  44   B  Build: bitmaps, HyperLogLog, geospatial          30 h
  45   B  Build: Sentinel                                  25 h
  46      Security: AUTH, ACLs, and TLS                     2 h
  47   B  Build: the security and admin surface            25 h
  48   B  Build: the compatibility sweep                   45 h   ← the acceptance test
```

**Total ≈700 h** — about 7 months at 25 h/week, or 17 at 10 h/week. Nothing in it is optional; the parts that were optional in earlier editions of this plan are exactly the parts that separate "a Redis-like server" from "a Redis".



## Time budget

Hours are **focused working hours**, not elapsed time. The ratios matter more than the absolutes — if chapter 30 takes 3× the estimate, that's normal; if chapter 7 does, something is wrong with your setup and you should ask.

| Part | Chapters | Read | Build | Total |
|---|---|---|---|---|
| Front matter + "How to study" | — | 1 h | — | **1 h** |
| **I — Orientation** | 1–2 | 4 h | — | **4 h** |
| **II — The server core** | 3–7 | 16 h | 36 h | **52 h** |
| **III — Keyspace, strings, expiry** | 8–13 | 11 h | 41 h | **52 h** |
| **IV — Data structures and collections** | 14–19 | 9 h | 111 h | **120 h** |
| **V — Persistence and eviction** | 20–26 | 13 h | 76 h | **89 h** |
| **VI — Replication** | 27–30 | 10 h | 55 h | **65 h** |
| **VII — Transactions and cluster** | 31–36 | 11 h | 86 h | **97 h** |
| **VIII — Production** | 37–38 | 4 h | 15 h | **19 h** |
| **IX — The rest of real Redis** | 39–48 | 11 h | 190 h | **201 h** |
| Appendices A–I | — | lookup | — | — |
| | | | | |
| **TOTAL** | 1–48 | ≈90 h | ≈610 h | **≈700 h** |

≈7 months at 25 h/week; ≈17 months at 10 h/week.

**Where the time actually goes:**

- **Part IX is 201 h**, nearly a third of the project, and it is entirely made of things a "build your own Redis" project normally declares out of scope. That is the point: a replacement is judged on its worst-supported command, not its best.
- **Chapters 30 and 36 — replication and cluster — are 114 h**, and they are the two subsystems almost every tutorial skips. They are also what interviewers actually ask about.
- **Chapter 23, persistence, is 53 h** and *harder in Go than in C*, because you cannot `fork()`. Snapshot-while-serving without copy-on-write is the most original engineering problem here (§20.5).
- **Chapter 17, the data-structure libraries, is 55 h** — the computer-science core. Everything after chapter 19 is systems engineering.
- **Chapter 48, the compatibility sweep, is 45 h** and will feel like the least glamorous work in the book. It is the only chapter that can tell you whether the previous 655 hours produced a replacement or an homage.
- Reading is under a seventh of total time, and a third of *that* is the source tours, not prose. This is a building project.


## Checkpoint demos

| After chapter | Demo |
|---|---|
| **7** | `redis-cli -p 7379` works against DiceMe: pipelined, 1000 concurrent clients, `redis-benchmark -P 16` clean, `FOOBAR` gets a proper error |
| **11** | `SET k v EX 2`, watch `TTL` count down, key vanishes on an **idle** server; `OBJECT ENCODING` shows `int`/`embstr`/`raw` correctly |
| **19** | `ZADD`/`ZRANGEBYSCORE` on 1M members; `OBJECT ENCODING` flips `listpack` → `skiplist` at exactly the configured threshold |
| **23** | `kill -9` mid-benchmark, restart, nothing acknowledged is lost (AOF always) or ≤1 s is lost (everysec) |
| **26** | Fill past maxmemory; approximate LRU keeps the hot set resident while cold keys die; plot the eviction-age histogram |
| **30** | Writes on master appear on replica in <10 ms; cut the link mid-stream, reconnect, **partial** resync — the logs prove no RDB was transferred |
| **33** | Two clients race `WATCH`/`MULTI`/`EXEC`, exactly one wins; `BLPOP` wakes the instant another client pushes |
| **36** | 6-node cluster (3 masters + 3 replicas), `kill -9` a master, automatic failover, `redis-cli -c` follows redirects transparently |
| **40** | A distributed-lock Lua script from a real library (`redlock`, or your own rate limiter) runs unmodified; a runaway script is killable |
| **42** | A consumer-group job queue: three workers, kill one mid-work, `XAUTOCLAIM` reassigns its pending entries, nothing is lost |
| **44** | A HyperLogLog written by DiceMe is counted correctly by real `redis-server` after `DUMP`/`RESTORE` |
| **45** | Three sentinels, `kill -9` the master, a client that only knows the sentinels keeps writing |
| **47** | An ACL-restricted user is denied by command, by key pattern, and by channel — with byte-exact error texts; replication runs over TLS |
| **48** | `go-redis`, `redis-py` and `node-redis` run their own integration suites against DiceMe and pass; `make compat` prints the scoreboard |

**On shortening the path.** You can stop at any checkpoint and have something real — chapter 23 leaves you a durable single-node cache, chapter 33 a genuinely useful one. But this book's target is a replacement, and a replacement has no partial credit: the first client library that sends `HELLO 3` or `EVAL` against a server that lacks them has found the edge. If you cut, cut deliberately, write down what you cut, and stop calling the result a replacement.


## Reading C source when you write Go

The Redis codebase is famously readable C, but three idioms trip up Go programmers:

1. **Intrusive data structures and `void*`.** A `listNode` holds `void *value`; a dict entry holds untyped key and value pointers. In Go you'll use ordinary typed structs — the *algorithm* is what you're extracting, not the memory layout.
2. **Flags packed into ints.** `CLIENT_MULTI | CLIENT_DIRTY_CAS` — grep the `#define` blocks at the top of `server.h` whenever you meet a constant. Keep `server.h` open in a tab permanently; it is the schema of the entire program: `struct client`, `struct redisDb`, `struct redisServer` are the three types everything else orbits.
3. **Error handling by return code + `goto err`.** Translate mentally to `if err != nil` and move on; don't study the cleanup ladders.

## When you're stuck

In order — do not skip to the bottom:

1. **Re-read the self-check questions** for that chapter. They're diagnostic: the one you can't answer names your gap.
2. **Ask the oracle.** Run the same command sequence against real Redis and diff output byte-for-byte (§6.4). Half your bugs are semantics you guessed instead of checked.
3. **Read the real Redis test** for that behavior — `tests/unit/` and `tests/integration/` in the redis repo. `tests/unit/expire.tcl` is a specification of expiry disguised as a test file; `tests/integration/replication-psync.tcl` is the same for partial resync.
4. **Read the real implementation** at the anchor in Appendix C.
5. **Add logging and run it.** Replication and cluster bugs are almost never reasoned out from source; they're observed. See ch. 28.

Timebox rabbit holes to two hours: write the question down, move on, come back.

## What this book deliberately does *not* cover

Being explicit so you know the edges. This list is short on purpose: everything else that a client can observe is in scope, because the target is a replacement.

- **The modules ABI** (`module.c`) — `MODULE LOAD` takes a C shared object compiled against Redis's internal ABI. A Go server cannot load one, at any effort level. That rules out RedisJSON, RediSearch, RedisTimeSeries and RedisBloom, and it is the one genuine, permanent gap (§48.4).
- **A byte-compatible cluster bus.** You build the cluster protocol's logic over your own node-to-node encoding (ch. 36), so an all-DiceMe cluster is complete but a *mixed* DiceMe/Redis cluster is not. Clients never speak the bus, so this does not affect a client-facing replacement — but §48.4 makes you write it down.
- **The 8.x internals as a build target** — `kvstore`, `ebuckets`, `kvobj`, `client_comp.c`, `defrag.c`. You read them for contrast; you build the classic model, which has identical observable semantics.
- **io_uring and platform-exotic I/O** — ch. 3 covers kqueue/epoll conceptually; you build on Go's netpoller.

---
---

# PART I — ORIENTATION

Why this system is shaped the way it is, and the one architectural decision you must make before writing any code.

---

# Chapter 1 — What Redis actually is

> **Time:** 2 h · **Before you start:** Nothing.
>
> **You're done when:** You can answer the chapter-1 self-check from memory.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Introduction to Redis" (`redis.io/docs`), then the command pages `redis.io/commands/{set,expire,lpush,zadd}` — read the **complexity** line on each | 45 min |
> | "Latency Numbers Every Programmer Should Know" — `gist.github.com/jboner/2841832` | 10 min |

## 1.1 The one bet

> **Redis bets that your working set fits in RAM, and that a single CPU core executing simple operations on in-memory data structures can outrun any design that touches disk or takes locks.**

Everything else — the event loop, the encodings, the forking persistence, the refusal to grow a query language — follows from taking that bet seriously.

The numbers that make the bet pay:

```
L1 cache reference                          0.5 ns
Main memory reference                       100 ns
Compress 1 KB with a fast codec           2,000 ns    2 µs
Send 1 KB over 1 Gbps network            10,000 ns   10 µs
SSD random read                         150,000 ns  150 µs
fsync() to SSD                        1,000,000 ns    1 ms
Round trip within a datacenter          500,000 ns  0.5 ms
```

Design consequences that follow directly:

- **A GET is ~100 ns of actual work** (one hash lookup) wrapped in 50–500 µs of network round trip. The server is never the bottleneck for one client; the network is. Therefore **pipelining** (§4.6) matters more than server micro-optimization, and one core really does serve 100k+ ops/sec.
- **fsync costs ~1 ms** — 10,000× the operation it protects. Persistence must be decoupled from the command path: AOF buffers in memory and fsyncs on a policy (§20.3), never inline.
- **Memory is the budget.** At ~100 bytes of overhead per key, 100M keys is 10 GB before any data. Hence the obsessive small-object encodings (§15.4, §8.2): a hash of 50 short fields is one packed byte array, not 50 heap objects.
- **One core, no locks** means every command is atomic by construction. `INCR` is atomic not through clever locking but because nothing else can possibly run concurrently. This is the deepest simplification in the design — and in Go you will have to *choose* it deliberately rather than inheriting it from C's single thread (§2.4). Your current code already lives this way by accident: the kqueue loop executes commands inline, one at a time.

## 1.2 A data-structure server, not a cache

The correct one-line description: **a server that exposes data structures over a wire protocol.** `LPUSH` is `list.insert(0, x)`; `ZRANGEBYSCORE` is a range query on an ordered map; `HSET` is `map[field] = value` inside `map[key]`.

This framing explains:

- **Commands are the API and there is no query planner.** Each command's complexity is documented and the user is the planner. `KEYS *` is O(N) and blocks the server for its whole runtime — and that's *your* fault, says Redis; the docs told you. (`SCAN` is the apology — §15.3.)
- **Atomicity is per-command**, plus MULTI/EXEC batches. One thread, one command at a time; each command sees a consistent world and leaves one.
- **Five-plus physical representations per logical type.** The contract is the structure's *behavior*, so the server freely swaps representation as data grows (§8.2). A 5-field hash is a flat byte array; a 5M-field hash is a real hash table. Same commands, same semantics, 10× memory difference.

## 1.3 What "production-grade" means for this project

Real Redis is production-grade because of what happens at the edges, not the center. A GET/SET server is a weekend — you've already had that weekend. These are the edges, and they are the actual syllabus:

| Edge | Chapter |
|---|---|
| A key expires *during* a read — who deletes it, and what may a **replica** do differently? | 9 |
| Memory is full — which key dies, and how do you choose without scanning everything? | 24 |
| `kill -9` mid-write — what's on disk, what loads at boot, exactly how much is lost? | 20–21 |
| The replica's TCP link drops for 3 seconds — full resync or continue from an offset? | 27 |
| The master dies — who notices, who decides, who promotes, who un-promotes the old master when it returns? | 34, 37 |
| A client WATCHes a key that expires before EXEC — does the transaction run? | 31 |
| A client blocks on BLPOP and another pushes — exactly when, in the event loop, does it wake? | 31 |
| A slow client can't drain replies as fast as you produce them — whose memory fills, and what pops first? | 38 |
| A script loops forever — who can still be served, and what may they send? | 39 |
| A consumer of a stream group dies holding ten unacknowledged entries — who gets them, and when? | 41 |
| A user is allowed `GET` but not on this key — where in dispatch is that decided? | 46 |

Every one of these has a precise, tested answer in the real source. Building to those answers — not to "seems to work" — is the project.

## 1.4 When Redis blocks, and why that's the cardinal sin

One thread means one crime: **anything O(N) on the command path stalls every client.** The codebase is shaped by avoiding it:

- Deleting a 10M-entry hash would block for seconds → **lazy free**: unlink the key now, hand the object to a background thread (`lazyfree.c`, §2.2).
- Rehashing a 100M-key dict at once would block → **incremental rehash**: a few buckets per operation (§15.2).
- Expiring a million dead keys at once would block → **sampling with a time budget** (§9.3) — the budget your `DeleteExpiredKeys` is missing (audit #11).
- Saving 10 GB inline would block for minutes → **fork + copy-on-write**: the child sees a frozen snapshot for free (§20.2). The one trick Go cannot copy; §20.5 is what to do instead.
- `KEYS` blocks → **SCAN**: a cursor iterator doing O(1) work per call that still guarantees every key present throughout the scan is returned at least once, *even across rehashes* (§15.3 — the reverse-binary cursor, the cleverest small algorithm in Redis).

The pattern, once: **turn every O(N) job into (a) incremental O(1) steps piggybacked on other work, (b) a background thread that never touches shared state, or (c) a forked child that owns a frozen copy.** You'll use (a) and (b) in Go; (c) you must re-invent.

## 1.5 What Redis refuses to be

Knowing the refusals prevents accidentally building them:

- **Not strongly consistent.** Replication is asynchronous; an acknowledged write can vanish in a failover. `WAIT` gives synchronous *replication*, not consensus — there is no Raft here, deliberately. Cluster failover can lose the tail of writes. If you want linearizable, that's the ConsulMe project sitting in the next directory.
- **Not a disk database.** RDB/AOF make memory durable; they never make the dataset larger than memory.
- **Not multi-tenant safe.** One client with `KEYS *` or a 512 MB value hurts everyone. Limits exist (§28), but the model is "trusted clients."
- **Not transparent.** Encodings, complexity, and memory cost are all user-visible (`OBJECT ENCODING`, `MEMORY USAGE`, `DEBUG SLEEP`). The user is expected to care.

## Self-check — Chapter 1

1. A GET takes ~100 ns of server work. Why does pipelining still 10× a client's throughput?
2. Why is `INCR` atomic in Redis without any lock? What has to be true of your Go design to keep that?
3. Name four O(N) jobs and the specific trick that keeps each off the command path.
4. Why does Redis document big-O complexity on every command when SQL databases don't?
5. What does `WAIT` guarantee, and what does it *not* guarantee that Raft would?
6. The same logical hash is stored two physically different ways at 5 fields vs 5M fields. What is the contract that makes this legal?

---

# Chapter 2 — Threads in a "single-threaded" server

> **Time:** 2 h · **Before you start:** Chapter 1.
>
> **You're done when:** You have written the §2.4 architecture decision in your own words in `NOTES.md`. Everything you build later assumes it.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `bio.c` — all of it (~350 lines): the job queue Redis trusts with fsync | 45 min |
> | `lazyfree.c`: `lazyfreeGetFreeEffort` + the UNLINK path | 30 min |

## 2.1 The truth table

"Single-threaded Redis" means: **one thread executes commands.** Around it:

| Threads | Job | Why it's safe |
|---|---|---|
| main | event loop + all command execution | owns all data |
| bio ×3 (`bio.c`) | background **fsync**, background **close** (of old AOF/RDB fds — close of a big file can block in the kernel!), background **lazy-free** | operate on handed-off resources the main thread no longer references |
| io-threads ×N (optional, 6.0+) | parse input / write output for batches of clients | main thread stops, threads run on *disjoint clients*, main thread resumes — fan-out/fan-in, never concurrent with execution |
| fork children | RDB save, AOF rewrite | separate process, COW view |

The design rule extractable from all four rows: **threads never share mutable data structures; they receive ownership of work items through a queue.** `bio.c` is a textbook worker pool: mutex + condition variable + job list, jobs like "fsync fd 7" or "free this object graph" whose inputs the main thread has already disowned.

## 2.2 Lazy freeing

`UNLINK` (and `DEL` under `lazyfree-lazy-user-del`, plus eviction/expiry under their lazyfree configs) removes the key from the dict — O(1), the key is *gone* observably — and, if the value's free-effort exceeds a threshold (64), hands the orphaned object to bio for freeing. In Go, **the GC is your lazy-free thread**: drop the reference, done. You inherit this entire mechanism for free — but note *why* it exists, because it's the same §1.4 principle, and note the one place it still bites Go: a multi-GB value's GC work happens *sometime*, as marking cost; if you see latency spikes after mass deletes, you've found it.

## 2.3 io-threads

Two generations of this feature exist, and knowing both is the point.

**Redis 6.0–7.x (the classic design, and the one most writing describes).** With `io-threads 4`, the main loop collects the clients with pending reads, wakes the threads to read+parse them in parallel, **waits on a barrier**, then executes every parsed command serially on the main thread; symmetrically for writes. Execution stays single-threaded — the speedup is protocol I/O only, and it pays off when replies are large or clients are many. Fan-out/fan-in with a barrier, not concurrency.

**Redis 8.x (this tree).** The implementation moved out of `networking.c` into its own `iothread.c` and changed shape: each io thread now runs **its own event loop** and owns a set of *assigned* clients (`assignClientToIOThread`), rather than the main thread borrowing threads for a batch. Clients are handed back and forth across explicit queues — `enqueuePendingClientsToMainThread` / `enqueuePendingClienstToIOThreads` (the typo is in the source), each direction guarded by a per-thread mutex and an `eventNotifier`. Commands that must not run off-thread are detected by `isClientMustHandledByMainThread` and pinned. There is even command **prefetching** (`prefetchIOThreadCommands`) so the io thread warms the keys' cache lines before the main thread executes.

The invariant survives both generations, and it's the one to keep: **command execution is still serial on the main thread.** What changed is only who does the socket and parsing work, and how ownership is handed over — which is exactly the §2.1 rule (threads receive ownership through a queue; they never share mutable structures) applied harder.

In Go: your connection goroutines *already are* io-threads (parsing happens off-engine, in parallel, by construction) — you got the whole feature for free from the architecture. Say so in the README, and if you want the 8.x lesson too, add the prefetch idea: the connection goroutine can look up the command's keys and touch them before handing the command to the engine.

## 2.4 Choosing DiceMe's concurrency model — the decision record

The options, honestly weighed:

**A. Global mutex around the store.** Correct; simple; every command serializes anyway, so it's the engine model with extra steps and worse: blocking commands hold nothing but still need a registry, and you *will* forget the lock somewhere (`Rename`'s eviction reentry — audit #13 — becomes a deadlock candidate).

**B. Sharded locks (16 shards by key hash).** Parallel single-key ops. But: multi-key commands (MSET, SINTERSTORE, RENAME) need multi-shard lock ordering (deadlock discipline); `MULTI` needs *all* touched shards; `RANDOMKEY`/`SCAN`/`FLUSHALL`/`DBSIZE` need all 16; eviction accounting (`used_memory` across shards) races; `WATCH` needs cross-shard coordination. Every mechanism in ch. 9–13 grows a caveat. This is the DragonflyDB path — a legitimate *different* project, and a great "then what" (Closing), but it re-derives every semantic this book teaches.

**C. Engine goroutine (chosen).** Connections parse in parallel (free io-threads); one goroutine owns store+expires+watchers+blocked-registry+repl-backlog and `select`s over {command channel, cron ticker, shutdown}. Every invariant in this book holds by construction. Throughput ceiling: one core of command execution — *which is the real Redis's ceiling too.* When a benchmark saturates it, you've reproduced the real bottleneck; the fix is the real fix (Cluster).

Write this decision into `NOTES.md` in your own words before ch. 7. It's the whole architecture.

## Self-check — Chapter 2

1. The complete list of what bio threads do. Why is *close* on the list?
2. State the ownership rule that makes all Redis threading safe.
3. What replaces lazy-free in Go, and what residual cost remains?
4. Why do io-threads not violate single-threaded execution? Which barrier enforces it?
5. For sharded locks, name four features that grow caveats and the caveat for each.
6. What is the engine model's throughput ceiling, and why is hitting it a feature of the learning project?

---
---

# PART II — THE SERVER CORE

Everything between the socket and the command table. You finish this part with a server `redis-benchmark` can hammer.

---

# Chapter 3 — The event loop

> **Time:** 5 h · **Before you start:** Chapter 2.
>
> **You're done when:** You can say where in the loop a command executes and where its reply reaches the socket.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `ae.c` + `ae.h` — the whole thing; the cleanest event loop you'll ever read. Entry: `aeMain`, `aeProcessEvents`. | 1 h |
> | `ae_kqueue.c` — you wrote one of these (`server/async_tcp.go`); compare choices line by line | 20 min |
> | "The C10K problem", Dan Kegel — `kegel.com/c10k.html`; the document that named the design space | 30 min |

## 3.1 The problem the event loop solves

10,000 connected clients; at any instant ~50 have data waiting. Thread-per-connection burns 10,000 stacks and context-switches to discover 9,950 have nothing to say. The alternative:

1. Make every socket **non-blocking** — reads/writes return immediately with what's available, or `EAGAIN`.
2. Ask the kernel which fds are ready: `kqueue` (your macOS), `epoll` (Linux), `select`/`poll` (portable, O(n), historical).
3. Loop: wait → for each ready fd do the non-blocking work → repeat.

One thread, no locks, O(ready) per iteration. This is the **reactor pattern**; `ae.c` is its minimal implementation:

```
aeMain (ae.c):
    while !stop:
        aeProcessEvents:
            timeout = time until nearest timer
            beforeSleep()                      ← flush AOF, unblock clients, write replies
            aeApiPoll(timeout)                 ← kqueue/epoll wait
            for each ready fd: call its read/write handler
            process due time events            ← serverCron
```

Two event kinds — the whole model:

- **File events**: fd readable/writable; handlers registered per fd (`aeCreateFileEvent`).
- **Time events**: "run `serverCron` every 1/hz seconds"; the nearest deadline sets the poll timeout, so an idle loop sleeps *exactly* until the next timer.

That last sentence is audit finding #6: your loop passes a nil timespec to `Kevent`, so with no traffic there is no wakeup, and the expiry cron starves. `ae.c` never has this bug *structurally* — the timeout computation is part of the loop's definition, not an afterthought.

## 3.2 Why Redis stays single-threaded anyway

Even with the loop, Redis could hand command execution to worker threads. It doesn't, because:

- Work per command is ~1 µs; lock overhead plus cache-line bouncing would eat a large fraction of it.
- Atomicity comes free — no lock ordering, no deadlocks, no torn reads, ever.
- The bottleneck is network and memory bandwidth, not CPU. When a core saturates, the official answer is more instances (Cluster, ch. 34).

Where Redis *does* use threads — the complete list — is ch. 2: background fsync/close/free (`bio.c`), lazy freeing, and optional I/O threads that only read/parse input and write output, never execute.

## 3.3 The Go decision: you already have an event loop

Your raw kqueue loop works, and writing it taught you the reactor pattern — mission accomplished, keep the file as a trophy. But carrying it forward fights the platform:

- Go's runtime **already multiplexes goroutines onto epoll/kqueue** (the netpoller). `conn.Read` parks a ~4 KB goroutine; the netpoller wakes it. Same syscalls you make by hand, plus a scheduler you don't have to write.
- Raw syscalls forfeit the stdlib (`bufio`, `crypto/tls`, portability to Linux, where your `syscall.Kevent` code won't even compile) and win nothing measurable at any scale you'll test.

So ch. 7 makes the architecture explicit. This book's recommendation:

> **Goroutine-per-connection for I/O; one owner goroutine for the store.** Each connection goroutine reads bytes, runs the incremental RESP parser, and sends complete commands into one channel. A single **engine goroutine** receives commands, executes them against the store, and hands reply bytes back. The engine goroutine *is* Redis's main thread: per-command atomicity, zero locks on the data path, and `WATCH`/`MULTI`/blocking/eviction semantics all stay exactly as simple as they are in C.

§2.4 works the alternatives (global mutex, sharded locks, lock-per-key) and shows how each quietly breaks `MULTI`, `RANDOMKEY`, `FLUSHALL`, or eviction accounting. The channel *is* your event queue; `select` over {command channel, cron ticker, shutdown} *is* your `aeProcessEvents`.

## 3.4 Socket details that will bite you

- **`TCP_NODELAY`** — Nagle's algorithm delays small writes ~40 ms hoping to coalesce. `anet.c:anetEnableTcpNoDelay` disables it on every accepted connection; Go's `net.TCPConn` disables it by default (one point to the stdlib — your raw-syscall path never did this, audit #16).
- **maxclients** — track connections against a limit (Redis default 10000); over it, reply `-ERR max number of clients reached` and close. Never silently drop.
- **Read buffer discipline** — a client may legally send a 512 MB bulk string. Grow the buffer as bytes arrive; enforce `proto-max-bulk-len` by erroring, never by pre-allocating.
- **Partial writes** — `write()` on a non-blocking socket may take 3 of your 10 KB. Loop, or let `bufio`/the writer goroutine handle it. Your `respond()` currently fires one `syscall.Write` and hopes (audit #16).
- **Output buffering** — replies queue per client; the loop drains them when the socket allows. Enforce **client-output-buffer-limits** or one slow subscriber OOMs you (§38.4). This is the most common way real Redis servers die in production.

## 3.5 The iteration, precisely

Memorize this ordering; three later chapters hang off it (blocking clients §31.3, AOF flush §20.3, replica feeding §27.5):

```
loop iteration:
  1. beforeSleep():                          server.c:beforeSleep
       - process clients ready to unblock (BLPOP wakeups from last iteration)
       - flush the AOF buffer to the file (fsync per policy)
       - stream pending replies to sockets
  2. poll(kqueue/epoll, timeout = nearest timer)
  3. ready file events:
       readable client → read → parse → execute command(s) → append reply to output buffer
  4. due time events:
       serverCron (server.c:serverCron): expiry cycle, rehash step, replication cron,
       save-point check, clientsCron (timeouts, buffer limits)…
```

The subtle point: **command execution happens inside the file-event handler**, synchronously, on the loop thread. "Handle readable client" *is* "run the command" — there is no queue between parse and execute in C Redis. Your engine-goroutine channel reintroduces a queue for good Go reasons, but the *semantics* (one at a time, in arrival order per client) must match.

## Self-check — Chapter 3

1. What does `kqueue`/`epoll_wait` actually return, and why is the loop O(ready) rather than O(clients)?
2. Why must the poll timeout equal the nearest timer deadline? Name the bug in your own repo caused by ignoring this.
3. Where in the iteration does a command execute? Where do its reply bytes touch the socket?
4. Why is goroutine-per-connection not a betrayal of the event-loop lesson in Go?
5. Name two features that quietly break under a lock-per-key store design.
6. A client sends one 512 MB `SET`. Walk your read path: what grows, what's checked, what must never be pre-allocated.

---

# Chapter 4 — RESP2 and RESP3: the wire protocol

> You write the parser and both serializers in chapter 7.
>
> **Time:** 4 h · **Before you start:** Chapter 3.
>
> **You're done when:** You can describe exactly what your parser must do with a half-received command, and why RESP3 is a chapter-7 concern rather than a later one.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "RESP protocol spec" — `redis.io/topics/protocol`; RESP2 carefully, RESP3 skim | 1 h |
> | `networking.c`: `processInputBuffer`, `processMultibulkBuffer`, `processInlineBuffer` — the shape of a real incremental parser | 45 min |

## 4.1 The shape of RESP2

First byte declares the type; `\r\n` terminates every line. Five types:

```
+OK\r\n                                       simple string (no CRLF inside)
-ERR unknown command 'FOO'\r\n                error (first word = error class)
:1000\r\n                                     integer (may be negative)
$5\r\nhello\r\n                               bulk string ($-1\r\n = null)
*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n     array (*-1\r\n = null array, *0\r\n = empty)
```

Requests are **always arrays of bulk strings** (or inline, §4.4). Replies use all five. Bulk strings are binary-safe — the length prefix means payloads may contain `\r\n`, zero bytes, JPEGs. This is why parsing by scanning for delimiters is wrong for bulk data, and why `readLength` needs to handle `-1` (audit #17).

## 4.2 The rules that make it good

Protocol-design lessons worth keeping:

- **Length-prefix data, delimit control.** Parsing never scans payload bytes: read the `$N` header, then exactly N bytes, then 2. O(1) per element after the header.
- **Human-typeable.** `telnet` in, type `PING`, get `+PONG`. Inline commands exist purely for this.
- **Type-tagged replies** — clients decode without out-of-band schema.
- **Errors are strings with a convention.** The first word (`ERR`, `WRONGTYPE`, `MOVED`, `ASK`, `NOAUTH`, `LOADING`, `BUSYKEY`…) is machine-parseable; the rest human. **Copy Redis's error texts byte-for-byte** — client libraries pattern-match them, and your diff harness (§6.4) will flag every deviation. `-WRONGTYPE Operation against a key holding the wrong kind of value` — exactly that.

## 4.3 The parser must be incremental — the audit-#1 fix

TCP is a stream. One `read()` may deliver half a command, exactly one, or three and a half (pipelining). The parser is therefore a **resumable state machine over an accumulating per-connection buffer**:

- Try to parse one complete command from the buffer.
- If the bytes run out mid-frame: return "incomplete" — a *normal* result, not an error — consuming nothing, and re-enter when more bytes arrive.
- If a full command parses: consume exactly its bytes, hand the command off, and immediately try again (pipelining).

`processMultibulkBuffer` keeps the resume state on the client struct: `multibulklen` (elements still expected) and `bulklen` (bytes still expected for the current element). The parse continues across event-loop iterations without re-scanning.

Your ch. 7 fix list for `core/resp.go`:

1. Every `readX` gets a "not enough data" return path; no indexing past `len(data)`, ever. Fuzz it (§7.2).
2. `readLength` handles `-1` (null bulk/array) and rejects garbage.
3. Enforce limits mid-parse: element count (>1M → `-ERR Protocol error: invalid multibulk length`), bulk length (> `proto-max-bulk-len`, default 512 MB), total buffer growth.
4. On protocol error: send the error, then **close the connection**. A desynced stream has no findable next-command boundary.

## 4.4 Inline commands

If the first byte isn't `*`, treat the line as a space-separated command: `PING\r\n`, `SET k v\r\n`. Twenty lines (`processInlineBuffer` adds quote handling; optional), and `nc`/`telnet` debugging stays pleasant forever. Note your AOF currently *depends* on inline-style splitting — that dependency dies in ch. 23 when the AOF becomes proper RESP arrays with binary-safe values (audit #4).

## 4.5 Replies and the output buffer

Commands never write to sockets. They append to the client's **output buffer** (`addReply*` family, networking.c); the loop drains buffers when sockets allow — mostly an eager write in `beforeSleep`, falling back to a writability event only when the socket backs up. Consequences:

- A command's latency never includes a slow client's socket.
- Output can accumulate → output buffer limits (§38.4).
- Per-client ordering is trivial.

In Go: a per-connection writer (goroutine draining a channel, or a mutex-guarded `bufio.Writer` flushed after each batch). The engine never blocks on any client's socket; a client over its buffer limit gets killed, same as Redis.

## 4.6 Pipelining

A client sends N commands without awaiting replies; the server executes in order, replies in order. Nothing special is needed *if* your parser loops (§4.3) and your writer preserves order. The win is pure latency arithmetic: 10,000 sequential SETs at 0.5 ms RTT = 5 s; batches of 100 = 50 ms. `redis-benchmark -P 16` is your standard load from ch. 7 onward — today it kills DiceMe inside a second (audit #2). Make "survives `-P 16` for 60 s" a ch. 7 done-when.

## 4.7 RESP3 (you build it in ch. 7)

`HELLO 3` upgrades the connection. Adds: typed maps (`%`), sets (`~`), doubles (`,`), booleans (`#`), big numbers (`(`), verbatim strings (`=`), one unified null (`_`), and **push frames** (`>`) — server-initiated messages (pub/sub, invalidation) that interleave with replies. RESP2 pub/sub fakes push with arrays; RESP3 makes it a frame type. **Build both serializers in chapter 7**, not later. Modern client libraries — redis-py 5, Lettuce, node-redis v4, go-redis — negotiate RESP3 on connect by default, so a server that only speaks RESP2 fails the very first handshake of a compatibility test. The design that makes this cheap: commands emit *logical* replies ("map", "double", "ok") and a per-connection serializer renders RESP2 or RESP3. If your eval functions return raw `[]byte` (as `core/eval.go` does today), RESP3 becomes a rewrite instead of a switch statement. Push frames arrive with pub/sub in ch. 33 and client tracking in ch. 48; the frame *type* is built here.

## Self-check — Chapter 4

1. Why length-prefix bulk strings instead of escaping? Two properties gained.
2. The buffer holds `*2\r\n$4\r\nLLEN\r\n$6\r\nmyl` — what exactly does your parser do, and what does it absolutely not do?
3. Why must a protocol error close the connection rather than resync to the next `*`?
4. Where do reply bytes live between execution and the socket write, and which two failure modes does that placement create?
5. What two server properties make pipelining "free"?
6. What do RESP3 push frames fix that RESP2 pub/sub fakes?

---

# Chapter 5 — Reading the source: the server core

> **Time:** 7 h · **Before you start:** Chapters 2, 3, 4.
>
> **You're done when:** You have run real Redis, and written down the call chain from socket to command by hand.


## Run it before reading it (45 min)

```bash
cd ~/Code/Learning/redis && make -j8          # builds src/redis-server, src/redis-cli
src/redis-server --port 6379 --daemonize no   # terminal 1
src/redis-cli                                  # terminal 2
> SET k v EX 10        ;  TTL k   ; OBJECT ENCODING k
> LPUSH l a b c        ;  OBJECT ENCODING l          # listpack
> DEBUG STRINGMATCH-LEN "*" x                        # DEBUG subcommands exist; explore DEBUG HELP
> DEBUG SLEEP 5                                      # watch a second redis-cli hang — single thread, proven
> MONITOR                                            # terminal 3: every command, live
src/redis-cli -3                                     # RESP3 session; compare HELLO output
redis-benchmark -q -n 100000 -P 16                   # your ch. 7 target numbers, on your laptop
```

What you steal: the *behavioral baseline*. Save `redis-benchmark` output to `NOTES.md` — DiceMe's numbers get compared against it at every build chapter.

## Boot & the three structs (2 h)

Read: `server.c:main` → `initServer` → `initListeners`; then in `server.h`: `struct redisServer` (skim, flag replication+aof fields), `struct redisDb`, `struct client`, the `CLIENT_*` and `CMD_*` flag blocks; `commands.def` for 30 seconds (it's generated from `commands/*.json` — the command table is *data*).
What you steal: boot ordering (§12.2); the client struct's field list (your `Client` needs ~⅓ of it, but know what you're omitting); command-table-as-data with flags (`write`, `denyoom`, `fast`, arity).

## The loop (1.5 h)

Read: `ae.c` whole (`aeMain`, `aeProcessEvents`, timer bookkeeping), `ae_kqueue.c`; `server.c:beforeSleep` top-to-bottom (skip TLS/cluster branches, list every job it does), `serverCron` (same treatment).
What you steal: the poll-timeout-from-nearest-timer rule (fixes audit #6); the beforeSleep-vs-cron split (§12.3) as your engine's `select` cases.

## Client I/O and the parser (2 h)

Read: `networking.c`: `createClient`, `readQueryFromClient`, `processInputBuffer`, `processMultibulkBuffer` (the resume-state fields!), `processInlineBuffer`, `addReply` + `addReplyBulk` + the output-buffer/reply-list machinery, `writeToClient`; skim `handleClientsWithPendingWrites`.
What you steal: the incremental-parse resume state (fixes audit #1/#2); reply-to-buffer-not-socket; protocol-error-then-close discipline.

## Dispatch & propagation (1.5 h)

Read: `server.c:processCommand` (every gate, in order: exists → auth → cluster redirect → OOM → loading → subscriber-mode → MULTI queue), `call()` (dirty counting, duration, propagation trigger), `alsoPropagate`/`propagateNow`.
What you steal: the gate sequence for your dispatcher; `(reply, effect)` propagation shape (§20.4); dirty counter placement.

# Chapter 6 — Project setup and conventions

> **Time:** 5 h · **Before you start:** Chapter 5.
>
> **You're done when:** `tests/harness/diff.sh` can run a command list against real Redis and DiceMe and show you a diff.


## 6.1 Rules

1. **Zero runtime dependencies** in the server (stdlib only; one exception, `gopher-lua`, arrives in ch. 40 for scripting; test-only deps fine). The point is building it.
2. **Every build chapter ends green**: its done-when script passes, `go test ./...` passes, `go vet` clean, and every *earlier* build chapter's done-when still passes (keep them as `make check-ch7`, `check-ch11`, … — regressions across chapters are the norm, not the exception).
3. **Error texts byte-identical to real Redis.** The diff harness enforces it.
4. **No eval touches storage directly** — everything through the keyspace API (§8.3). Enforced by review, or by making the store package-private with an exported API surface.

## 6.2 Layout (grows from your current tree)

```
diceMe/
├── main.go                    # flags, config load, engine start  (evolves)
├── config/                    # becomes: file parsing + CONFIG GET/SET registry
├── server/
│   ├── engine.go              # NEW ch. 7: the owner goroutine (§2.4)
│   ├── conn.go                # NEW ch. 7: accept loop, per-conn read/parse/write goroutines
│   ├── client.go              # NEW ch. 7: Client struct (buffers, flags, MULTI state, …)
│   ├── async_tcp.go           # kept as the museum piece; no longer wired to main
│   └── sync_tcp.go            # same
├── core/
│   ├── resp/                  # ch. 7: incremental decoder + reply serializer (RESP2, later 3)
│   ├── store/                 # ch. 11: redisDb{dict,expires}, keyspace API, choke point
│   ├── ds/                    # ch.17: dict/  listpack/  skiplist/  quicklist/  intset/
│   ├── cmd/                   # command table + eval functions per type: string.go list.go …
│   ├── expire/  evict/        # ch. 11 / ch. 26
│   ├── persist/               # ch. 23: rdb.go aof.go snapshot.go
│   ├── repl/                  # ch. 30: master.go replica.go backlog.go
│   └── cluster/               # ch. 36
├── tests/
│   ├── harness/               # §6.4 diff harness
│   └── chNN_test.sh           # each build chapter's done-when, executable
└── storm/                     # grows: mixed workloads, chaos scripts
```

Migration note, not big-bang: ch. 7 creates `engine/conn/client` and the `resp` package (porting your decoder's good parts), points `main.go` at them, and *deletes nothing*. Later build chapters move `core/*.go` contents into their packages as each is rebuilt.

## 6.3 Testing strategy per layer

| Layer | Strategy |
|---|---|
| ds/* | pure unit + property tests (rehash invariants, listpack round-trip, skiplist vs reference `sort` model, SCAN-guarantee test with forced rehash) — plus fuzz (`go test -fuzz`) on listpack and RESP |
| resp | table tests + **fuzz** (must never panic on any byte sequence — audit #1's grave) |
| cmd/store | script tests: run command list against engine in-process, assert replies; then the same list through the diff harness |
| persist | crash tests: `kill -9` the server at randomized points under load (storm), restart, assert the §20.6 matrix row |
| repl/cluster | multi-process integration: spawn N servers on ports, drive with redis-cli, assert convergence; deterministic seeds |

## 6.4 The diff harness — build first, 4 h

The most valuable tool in the project:

```bash
tests/harness/diff.sh commands.txt
#  starts real redis-server :6379 and dice :7379 (fresh, no persistence)
#  pipes each line via redis-cli --no-raw to BOTH; diffs stdout; reports first divergence
```

Plus a Go mode for pipelining/blocking cases (two `net.Conn`s, raw RESP bytes both ways, byte-diff). Every build chapter's command surface (Appendix B) gets a `commands.txt`. Nondeterminism (RANDOMKEY, SPOP, INFO, TTL-under-latency) is handled by a small allowlist of "normalize before diff" rules — build the allowlist as you hit them, keep it short and documented; every entry is a place your tests are weaker, so prefer seeding determinism (e.g., fixed TTLs, sorted SMEMBERS) where possible.

## 6.5 Tooling (do once, ~1 h)

`Makefile`: `build test vet fuzz check-chNN bench diff`. `bench` = `redis-benchmark -q -n 100000 -P 16 -p 7379` with the real-Redis column beside it. Pin Go version. `golangci-lint` optional. CI optional (this is a learning repo; the harness is your CI).

---

# Chapter 7 — Build: the server core, rebuilt

> **Time:** 31 h · **Before you start:** Chapters 2, 3, 4, 5, 6.
>
> **You're done when:** The done-when checks in this chapter pass: `redis-benchmark -P 16` runs clean, the RESP fuzzer finds no panic, and an idle server still expires keys.


**Goal**: DiceMe survives anything a client can throw at the *transport and protocol* layer, with the ch. 2 architecture underneath. No new commands — this chapter is plumbing, and it retires audit findings 1, 2, 5, 6, 9, 15, 16, 17.

## 7.1 The pieces

**`resp` package.** Incremental decoder per §4.3: `Decoder{buf []byte; ...}` with `Feed(data)` and `Next() (args [][]byte, err error)` returning `ErrIncomplete` freely. Port your `readX` functions' logic; kill their panics; add `-1` lengths, limits, and inline commands.

Serializer side, and this is the part that pays for itself three times: build the **logical-reply layer** (§4.7) and *two* renderers. Evals call `ReplyOK / ReplyErr / ReplyBulk / ReplyInt / ReplyDouble / ReplyBool / ReplyNil / ReplyArray / ReplyMap / ReplySet / ReplyPush` on a `RespWriter` and never format RESP by hand; the writer holds the connection's protocol version and emits RESP2 or RESP3 accordingly (a RESP3 map degrades to a flat RESP2 array, a double to a bulk string, a bool to `:0`/`:1`, a null to `$-1` — the degradation table *is* the RESP2 renderer). `HELLO [2|3] [AUTH u p] [SETNAME n]` flips the version and returns the server map. Doing this now costs about six hours; doing it in chapter 48 costs a rewrite of every eval function you will have written by then, which is all of them.

**`Client`.** conn, decoder, reply writer + output channel, id, name, flags (bitfield, like you did TypeEncoding), createdAt/lastCmd — plus the fields you *know* are coming (MULTI queue, watched list, blocked state) declared but unused. The fake-client constructor (`NewDetachedClient()`) exists now, tested by feeding it commands directly — AOF-load and replication plug into it later.

**`engine`.** One goroutine:

```go
for {
    select {
    case req := <-e.requests:            // {client, args, done chan}
        reply := e.dispatch(req)          // command table lookup → gates → eval
        req.done <- reply
    case <-e.cronTick.C:                  // 100ms — expiry/rehash/etc live here later
        e.cron()
    case <-e.shutdown:
        e.drainAndExit()                  // finish in-flight, flush AOF (later), close
        return
    }
}
```

**Command table as data** (ch. 5's steal):

```go
var commandTable = map[string]Command{
  "SET": {Eval: evalSET, Arity: -3, Flags: CmdWrite | CmdDenyOOM},
  "GET": {Eval: evalGET, Arity: 2, Flags: CmdReadOnly | CmdFast},
  ...
}
```

Dispatch gates in `processCommand` order (ch. 5): unknown → `-ERR unknown command 'x', with args beginning with:` (audit #5 dies) · arity (negative = at-least, Redis convention) · [later: auth, OOM, loading, subscriber-mode, MULTI]. Case-insensitive lookup (uppercase once).

**Connection lifecycle.** Accept goroutine (maxclients gate) → per-conn: reader goroutine (read→Feed→Next loop→engine channel) + writer (drain reply channel→`bufio.Writer`→flush per batch, partial-write-safe by construction). Disconnect cleanup: deregister everything (a function that grows all project long — get its shape right now). Graceful shutdown: signal → stop accepting → engine drains → connections closed → exit; second signal = immediate. `wg` accounting fixed (audit #15).

**Config.** Parse a minimal `dice.conf` (key value lines, `#` comments — Redis's format) + flags override + defaults: port, bind, unixsocket, tcp-keepalive, timeout, maxclients, proto-max-bulk-len, appendonly(later), save(later)… `unixsocket` costs four lines here (`net.Listen("unix", …)` behind the same accept loop) and is standard in every colocated deployment, so do it now rather than discovering it in ch. 48. Registry pattern so `CONFIG GET/SET` (ch. 11) is a map iteration, and configs are declared once with type+default+validator.

**Commands carried over**: PING, ECHO (new, trivial), QUIT, HELLO (2 and 3 — see the serializer above), SET/GET/DEL/TTL/EXPIRE/etc. keep working through the new path — port the evals as-is; ch. 11 rewrites their semantics. Drop LRU and SLEEP from the table; re-add SLEEP as `DEBUG SLEEP` (build `DEBUG` now with SLEEP + JMAP-ish stub — you'll grow it every build chapter).

## 7.2 Done-when

```bash
redis-cli -p 7379 PING                          # PONG
redis-cli -p 7379 FOOBAR a b                    # ERR unknown command 'FOOBAR', …
redis-benchmark -p 7379 -q -n 100000 -P 16 -t ping,set,get   # clean, 0 errors — audit #2 dead
printf 'PING\r\n' | nc localhost 7379           # inline works
redis-cli -3 -p 7379 HELLO 3                    # RESP3 map reply; diff its shape against real redis
redis-cli -3 -p 7379 PING                       # the whole session speaks RESP3 afterwards
# partial-frame torture: send a SET split into 1-byte writes with 10ms gaps — correct reply
go test -fuzz=FuzzDecode -fuzztime=60s ./core/resp/          # no panics — audit #1 dead
# 1000 concurrent clients (storm) for 60s; kill client processes randomly; server RSS stable, no goroutine leak (pprof)
# ^C: drains, exits 0.  kill -9 storm mid-run: server unaffected.
# idle server with a 1s-TTL key set → key gone after ~1s (cron runs without traffic — audit #6 dead)
```

## 7.3 Traps

- Deadlock: engine replying into a full client channel while that client's reader is blocked sending to the engine. Rule: writer channels are buffered + engine *never* blocks on them (overflow = kill client, §38.4 semantics arrive early).
- `[][]byte` aliasing: decoder buffers reused → args must be copied (or ownership documented) before crossing the channel. This bug is silent data corruption under pipelining; write the test.
- Goroutine leaks on disconnect — `pprof/goroutine` after the churn test, count must return to baseline.
- Don't build RESP3, sharding, or io-thread cleverness now. The chapter is boring on purpose; boring here is velocity everywhere else.

---

---
---

# PART III — KEYSPACE, STRINGS, AND EXPIRY

The object model, the keyspace choke point, and the expiry contract — then the first milestone build, and the architecture chapters that only make sense once you've built it.

---

# Chapter 8 — The object model and the keyspace

> **Time:** 2 h · **Before you start:** Chapter 7.
>
> **You're done when:** You can list the five cross-cutting concerns that hang off the keyspace write path.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `object.c`: `createObject`, `tryObjectEncoding`, `objectCommand` | 45 min |
> | `db.c`: `lookupKeyReadWithFlags`, `dbAdd`, `setKey` — the keyspace API every type file calls | 45 min |

## 8.1 robj: one header for every value

```c
typedef struct redisObject {
    unsigned type:4;        // STRING, LIST, SET, ZSET, HASH, STREAM
    unsigned encoding:4;    // how it's physically stored
    unsigned lru:24;        // LRU clock OR LFU counter+time (policy decides)
    int refcount;
    void *ptr;
} robj;
```

Your `Obj{TypeEncoding uint8, LastAccessedAt uint32, Value interface{}}` is this, minus refcount. The nibble-packing in `core/typeencoding.go` mirrors the C bitfields exactly — keep it.

- **type** is the user-visible contract (`TYPE` command, `WRONGTYPE` errors). **encoding** is the private representation. Commands check type; type internals check encoding.
- **refcount** exists in C for shared objects and memory management. In Go the GC handles lifetime — **skip refcount** (your snapshot discipline in §23.3 replaces its one load-bearing use). C Redis pre-creates the integers 0–9999 as shared objects (`shared.integers`) so every `:5` reply and small value costs nothing; in Go, interning small-int strings is an optional micro-optimization — measure with ch. 38's benchmark before bothering.
- **lru** — 24 bits holding either the LRU clock (seconds, wraps ~194 days — your `getCurrentClock` already masks to 24 bits) or, under LFU policy, 16 bits of decay-time + 8 bits of log-counter (§24.3). Same field, two interpretations, chosen by `maxmemory-policy`.

## 8.2 The encoding matrix — learn it cold

| Type | Small encoding | Threshold (config, defaults) | Large encoding |
|---|---|---|---|
| string | `int` (is an integer), `embstr` (≤44 B, one allocation) | — | `raw` |
| list | `listpack` | `list-max-listpack-size` **-2** | `quicklist` |
| hash | `listpack` (field,value,field,value…) | `hash-max-listpack-entries` **512**, `-value` 64 B | `hashtable` (dict) |
| set | `intset` (all ints) → `listpack` | `set-max-intset-entries` 512, `set-max-listpack-entries` 128, `-value` 64 B | `hashtable` (dict) |
| zset | `listpack` (member,score…) | `zset-max-listpack-entries` 128, `-value` 64 B | `skiplist` + dict |

Two of those defaults deserve a second look, because they're the ones people misquote (including older editions of this table):

- **`list-max-listpack-size` is `-2`, not a count.** Negative values select a *size* limit per quicklist node rather than an entry count: -1 = 4 KB, **-2 = 8 KB** (the default), -3 = 16 KB, -4 = 32 KB, -5 = 64 KB. A positive value means "N entries per node." So a list converts to quicklist not at a fixed length but when the packed bytes stop fitting in 8 KB — which is the more honest limit, since what actually hurts is the `memmove`, and that scales with bytes.
- **`hash-max-listpack-entries` is 512**, not 128 like the others. Hashes get a bigger allowance because a listpack hash stores field and value adjacently, so lookups have good locality and the linear scan stays cheap further out than it does for a set or zset.

Always check the running server rather than trusting a table — `CONFIG GET *-max-*` on your reference `redis-server` is the ground truth, and your ch. 19 tests should read the thresholds from config rather than hardcoding them.

Conversion rules: **one-way** (never convert back down when items are removed — avoiding thrash beats reclaiming bytes), triggered on the *insert or growth* that crosses either the entry-count or the value-length threshold. `OBJECT ENCODING key` exposes the current encoding — it's how your tests prove conversions happen at exactly the configured boundary (a ch. 19 done-when).

`tryObjectEncoding` (object.c) is the string version: try parsing as int → `int` encoding; ≤44 bytes → `embstr`; else `raw`. Your `deduceTypeEncoding` already matches. One subtlety you're missing: **`APPEND` and `SETRANGE` must force `raw`** — mutating an int-encoded or embstr value in place is illegal (embstr is immutable by design; int has nothing to append to). ch. 11.

## 8.3 The keyspace API — one choke point

Every type file (`t_string.c`, `t_list.c`…) goes through `db.c`, never touching the dict directly:

- `lookupKeyRead(db, key)` — lookup **honoring expiry** (§9.2), counting hit/miss stats, updating LRU/LFU. May return NULL for an existing-but-expired key.
- `lookupKeyWrite(db, key)` — same plus expiry-deletion has write semantics (propagates, dirties).
- `dbAdd` / `setKey` / `dbDelete` — mutations, which also: bump the **dirty counter** (feeds RDB save points §20.2 and replication), fire **keyspace notifications** (§31.5), **signal watched keys** (§31.1 — `touchWatchedKey`), and **signal ready keys** for blocked clients (§31.3).

This is the architecture lesson of the chapter: **cross-cutting concerns attach to the keyspace choke point, not to 200 command implementations.** When you add WATCH in ch. 33, it's five lines in `setKey`, not a hunt through every command. Your current code half-has this (`core/store.go`) — ch. 11 formalizes it: *no eval function may touch `store`/`expires` directly.* (`evalKEYS`, `evalRENAME`, `evalPERSIST` currently do.)

## 8.4 Sixteen databases, not one

Redis has `databases 16` (`SELECT n`) — a legacy feature nobody designs around any more, and Cluster mode allows only db 0. It is tempting to build one `redisDb` and error on `SELECT 1`, and that is what a study project does.

Build the array. The cost is small and paid once: `redisDb` is already the right struct, the client carries a current-db index, and every keyspace call takes the db it operates on. The cost of *not* building it shows up everywhere later — `SELECTDB` opcodes in the RDB (§20.2), the `SELECT` the replication stream emits when the propagated command's db differs from the last one sent, `INFO keyspace`'s per-db lines, `MOVE`, `SWAPDB`, `COPY ... DB n`, `FLUSHALL` versus `FLUSHDB`, and a large fraction of the real test suite, which uses db 9 for its own isolation and will therefore refuse to run at all against a single-db server (§48.2).

So: `databases 16` config, an array of `redisDb`, per-client `db` index, and `SELECT` validating against the configured count. In cluster mode, reject `SELECT n` for n > 0 exactly as real Redis does. The struct still matters most — it is the unit replication and persistence iterate — but the multiplicity is not optional for a replacement.

## Self-check — Chapter 8

1. Why are type and encoding separate fields? Which one does `WRONGTYPE` check?
2. Why are encoding conversions one-way?
3. What five cross-cutting concerns hang off the keyspace write path? Where would WATCH live if you skipped the choke point?
4. Why must `APPEND` force `raw` encoding?
5. What are the two interpretations of the 24-bit `lru` field, and what chooses between them?
6. Name four subsystems that change shape if you build one database instead of sixteen.

---

# Chapter 9 — Expiration

> **Time:** 2 h · **Before you start:** Chapter 8.
>
> **You're done when:** You can explain why a replica never expires a key on its own.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `expire.c`: top-of-file comment + `activeExpireCycle` (the modern one walks ebuckets; read the *algorithm description* in the comment, which still describes the classic sampling contract) | 45 min |
> | `db.c:expireIfNeeded` — the lazy path and the replica rule; read the comment block above it three times | 30 min |
> | `tests/unit/expire.tcl` — expiry semantics as executable spec; plus `redis.io/commands/expire` (the "Appendix: how Redis expires keys" section is the official writeup of this chapter) | 30 min |

## 9.1 The contract

- A key with a TTL in the past **must never be observable** — not by GET, not by EXISTS, TYPE, TTL, RANDOMKEY, SCAN, KEYS. To every reader it is already gone, regardless of whether the delete has physically happened.
- TTLs are **absolute Unix-millisecond timestamps** internally (`expires[key] = now + ttl` — yours already does this). `EXPIRE 10` and `PEXPIREAT <ts>` are two spellings of one storage.
- `SET` on an existing key **discards** its TTL (yours does — correct). `GETSET`/`SET ... KEEPTTL` variations preserve it. `RENAME` carries the TTL. `PERSIST` removes it and reports whether it did.
- `EXPIRE` with a **non-positive TTL deletes the key immediately** — semantically a `DEL`, and it *propagates* as one (§9.4). Audit #7: your `evalEXPIRE` sets a past timestamp instead, leaving a phantom that hangs around until sampled.
- Modern flags — `EXPIRE key sec [NX|XX|GT|LT]` (only-if-none, only-if-some, only-if-greater, only-if-less): ch. 11 stretch, trivial once storage is right.

## 9.2 Lazy expiry: expireIfNeeded

Every keyspace lookup funnels through one function:

```
expireIfNeeded(db, key):
    if no TTL or TTL in future: return VALID
    if server is a REPLICA:     return EXPIRED   // but DO NOT DELETE — §9.4
    delete key (sync or lazy-free)
    propagate DEL/UNLINK to replicas + AOF
    fire "expired" keyspace notification
    return EXPIRED
```

The subtleties that distinguish a toy:

1. **The delete is a write, with full write side-effects** — propagation, dirty counter, notifications, watched-key touch. Your current `Get → hasExpired → Del` silently drops all of those (fine today; fatal the day replication exists — this is *the* classic replication-divergence bug, Appendix F).
2. **One choke point.** GET, TYPE, TTL, EXISTS all pass through it. Your `evalGET`'s dead second check (audit #18) is what happens when expiry logic is sprinkled instead of funneled.
3. Commands that will *write* the key still check it (`lookupKeyWrite` → expired means "doesn't exist", so `SETNX` succeeds).

## 9.3 Active expiry: sampling with a budget

Lazy alone leaks: keys never touched again never die, yet hold memory. The active cycle (classic algorithm, `activeExpireCycle`):

```
every serverCron tick (10/sec), for each db:
    loop:
        sample up to 20 keys from the EXPIRES dict
        delete the expired ones (with full write side-effects)
        if expired fraction ≤ 25%: break        // statistically ~clean
        if time spent this cycle > budget: break // ~25% of 1/hz — HARD CAP
```

The two ideas: **sample the expires dict, not the keyspace** (yours does — the README's claim that it walks the store is stale), and the **time budget** (yours is missing — audit #11): the 25% rule alone loops arbitrarily long after mass expiry. The budget guarantees the cron never eats the loop; leftover work waits a tick. There's also a "fast cycle" variant (runs in `beforeSleep` with a 1 ms cap when the last cycle hit its budget) — optional.

The statistical claim worth understanding: if every sample of 20 shows ≤25% expired, then expired keys are ≤~25% of the expires dict with high probability — bounded *garbage ratio*, not zero garbage. Redis chooses "bounded waste, bounded latency" over "clean, unbounded pause." That trade *is* this chapter.

Modern note: 8.x replaced sampling with `ebuckets` — TTLs bucketed by deadline, so the cycle pops whole due-buckets instead of sampling. Better, and a good optional refactor once ch. 48 passes; build sampling first, because the observable semantics are identical and the budget discipline is the lesson.

## 9.4 Expiry × replication — the rule everyone gets wrong

The most important section in this chapter, and per-hour the most valuable in the book:

> **Replicas never expire keys on their own.** The master expires (lazily or actively) and **propagates an explicit `DEL`/`UNLINK`**. A replica's `expireIfNeeded` *hides* an over-due key from readers (returns "gone") but leaves it in memory until the master's DEL arrives.

Why: replica clocks differ from the master's; if each node expired independently, keyspaces diverge — then a failover promotes a replica whose "wrong" extra keys suddenly resurrect, or whose missing keys were still alive on the master. One source of truth for the *fact* of expiry; clock skew reduced to read-visibility jitter.

Two corollaries you will implement in ch. 30:

- The DEL the master propagates is the **canonical event**; AOF gets it too, so replaying the AOF reproduces the exact expiry timeline, independent of replay-time clock.
- **Writable-replica caveat** aside (don't build writable replicas), the replica *serving* a read of an over-due key returns nil while `DBSIZE` still counts it. The asymmetry is by design; write it into your tests.

## Self-check — Chapter 9

1. Why must expiry-on-read take the full write path (propagate, dirty, notify)? Name the failure if it doesn't.
2. Two reasons the active cycle samples `expires` and not the main dict?
3. What does the 25%-rule bound, statistically? What does the time budget bound? Why are both needed?
4. A replica's clock runs 30 s fast. Walk a key with 10 s TTL: what does each node store, hide, and delete, and when?
5. Why does `EXPIRE key -1` propagate as `DEL` rather than as `EXPIRE key -1`?

---

# Chapter 10 — Reading the source: keyspace and strings

> **Time:** 3 h · **Before you start:** Chapters 8 and 9.
>
> **You're done when:** You can name the one function every key lookup passes through, and what it does on a replica.


## Objects & strings (1.5 h)

Read: `object.c`: `createStringObject`, `tryObjectEncoding`, `getDecodedObject`, `objectCommandGetKey`; `t_string.c`: `setGenericCommand` (all flags — NX/XX/EX/PX/EXAT/KEEPTTL/GET), `incrDecrCommand` (overflow check!), `appendCommand` (encoding force + grow); `sds.c` skim for the growth policy.
What you steal: SET's full option matrix + error texts; INCR's `ERR increment or decrement would overflow` guard (audit #10); APPEND-forces-raw (§8.2).

## Keyspace & expiry (1.5 h)

Read: `db.c`: `lookupKeyReadWithFlags`, `lookupKeyWrite`, `setKey`, `dbGenericDelete`, `expireIfNeeded` + its comment, `scanGenericCommand`; `expire.c`: `activeExpireCycle` + `expireGenericCommand` (the NX/XX/GT/LT flags, the delete-on-past branch).
What you steal: the choke-point function set (§8.3); replica-hides-master-deletes (§9.4); the cycle's budget arithmetic; EXPIRE-past→DEL (audit #7).

# Chapter 11 — Build: keyspace, strings, and expiry, done right

> Your first milestone.
>
> **Time:** 41 h · **Before you start:** Chapters 8, 9, 10.
>
> **You're done when:** The done-when script in this chapter passes, and `tests/ch11_commands.txt` diffs clean against real Redis.


**Goal**: the string type and key-management surface, semantics byte-identical to Redis, expiry per ch. 9. Retires audit findings 7, 10, 11, 13, 14, 18.

## 11.1 The store package

`redisDb{dict map[string]*Obj; expires map[string]int64}` — Go map for now, **replaced by your dict in chapter 17** behind this same API (that's why the API exists):

```
LookupRead(key)  → *Obj      // expireIfNeeded inside; nil if missing/expired; LRU touch; hit/miss stats
LookupWrite(key) → *Obj      // same + write-path expiry semantics
Set(key, obj)    // dirty++, hooks: touchWatched (stub), signalReady (stub), notify (stub)
SetExpire(key, atMs) / GetExpire / RemoveExpire
Delete(key)      // full write hooks
Size(), RandomKey(), Scan(cursor, count, match)
```

Hooks are declared now, no-ops until chapters 23–33 fill them: **the choke point is this chapter's architectural deliverable.** Move `KeyspaceStat` into the db struct (fix the nil-map panics), plus `expires` count and hit/miss for `INFO`.

`expireIfNeeded` per §9.2 — including the `isReplica` parameter that currently always passes false, and the propagation hook that currently no-ops. Write it *right*, wire it *later*.

## 11.2 Command surface

Strings: `SET` (full matrix: NX XX GET EX PX EXAT PXAT KEEPTTL — mutual-exclusion errors included), the deprecated-but-everywhere legacy spellings `SETNX`/`SETEX`/`PSETEX`/`GETSET` (all four are one-liners over setGenericCommand once SET's matrix exists — that's the lesson), `GET`, `GETDEL`, `GETEX`, `APPEND` (forces raw!), `STRLEN`, `SETRANGE`/`GETRANGE`, `INCR`/`DECR`/`INCRBY`/`DECRBY` (overflow: `ERR increment or decrement would overflow`), `INCRBYFLOAT` (formatting: no trailing zeros — diff against real; and remember §20.4, it propagates as SET later), `MSET`/`MGET`/`MSETNX`.

Also `LCS` (7.0: longest common subsequence of two string keys, with `LEN`/`IDX`/`MINMATCHLEN`/`WITHMATCHLEN`) — required, and instructive: it is plain dynamic programming at **O(N×M) time _and_ memory**, so on two 1 MB strings it allocates a terabyte-scale matrix and real Redis simply refuses past a limit. Building it teaches the §1.2 lesson from the inside — the server hands you a sharp tool, documents the complexity, and trusts you.

Keys: `DEL`, `UNLINK` (alias for now — GC is lazy-free), `EXISTS` (with repeats — `EXISTS k k k` counts 3), `TYPE`, `TOUCH`, `COPY` (with `DB`/`REPLACE`; must deep-copy the value, and *not* the TTL unless asked), `RENAME`/`RENAMENX` (as a store-level swap, fixing audit #13; TTL travels; `no such key` error), `KEYS` (your own glob: `* ? [abc] \x` — port `stringmatchlen`, ~40 lines, audit #14), `SCAN` (with MATCH/COUNT/TYPE — cursor contract §15.3; with Go maps use the keys-snapshot trick temporarily, honest cursor arrives with the dict), `RANDOMKEY`, `TTL`/`PTTL` (rounding!), `EXPIRE`/`PEXPIRE`/`EXPIREAT`/`PEXPIREAT` (+ NX XX GT LT; past→delete, audit #7), `PERSIST`, `DBSIZE`, `FLUSHDB`/`FLUSHALL`, `OBJECT ENCODING|IDLETIME|FREQ|HELP`, `SELECT` (all 16, §8.4), `SWAPDB`, `MOVE`, `EXPIRETIME`/`PEXPIRETIME`, `DEBUG OBJECT|SLEEP|SET-ACTIVE-EXPIRE|JMAP|STRINGMATCH-LEN`.

Server: `CONFIG GET/SET/RESETSTAT` (registry-backed; glob patterns), `INFO` (server, clients, memory-approx, stats, keyspace sections — real section format, plus `commandstats`, `errorstats` and `latencystats`, because every Prometheus exporter parses those by name and a missing field is a broken dashboard), `CLIENT SETNAME|GETNAME|LIST|ID|INFO`, `TIME`.

And the **`COMMAND` introspection family**, which is not the trivia it looks like: `COMMAND` (full reply), `COMMAND COUNT`, `COMMAND INFO name…`, `COMMAND DOCS [name…]`, `COMMAND LIST`, `COMMAND GETKEYS cmd args…`. `redis-cli` calls `COMMAND DOCS` on startup to build its completion table; several client libraries call `COMMAND` to learn arity and key positions; `COMMAND GETKEYS` is what cluster proxies and your own ACL key checks (ch. 47) use to find which arguments are keys. Build the metadata as *data* in the ch. 7 command table — arity, flags, first/last/step key positions, ACL categories (filled in ch. 47) — and generate it from real Redis's `commands/*.json` rather than hand-typing it. That generator is 100 lines and removes an entire class of divergence.

## 11.3 Active expiry

Port §9.3 exactly: cron case in the engine ticker; 20-key samples from `expires`; 25% repeat rule; **time budget** = 25% of the tick interval (audit #11 dies); expired deletions go through `Delete` (full hooks — so when propagation lands in ch. 23/5, expiry propagates for free). `DEBUG SET-ACTIVE-EXPIRE 0` toggles it for tests.

## 11.4 Done-when

```bash
tests/harness/diff.sh tests/ch11_commands.txt      # ~400 lines covering every command+edge above: zero diffs
redis-cli -p 7379 SET k v EX 0                        # ERR invalid expire time in 'set' command
redis-cli -p 7379 EXPIRE k -1  → 1; EXISTS k → 0      # past-TTL deletes
redis-cli -p 7379 SET n 9223372036854775807; INCR n   # ERR increment or decrement would overflow
redis-cli -p 7379 SET s abc; APPEND s d; OBJECT ENCODING s   # raw
# SCAN never misses: script inserts 10k keys, SCANs with COUNT 10 while another client churns inserts/deletes
#   → every stable key appears ≥ once
# active expiry under budget: create 1M keys TTL 1s; measure max command latency during the die-off < 5ms
```

## 11.5 Traps

- `SET k v GET` on a wrong-type key errors *before* writing. `SET k v NX GET` combos: check real behavior, don't guess (the rules changed in 7.0 — GET is now allowed with NX).
- TTL rounding: `PTTL` exact ms; `TTL` rounds *to nearest second* — off-by-one diffs from the harness are the test working.
- `INCRBYFLOAT` output formatting (`%.17g` then trim) — the harness catches it; match it.
- SCAN's cursor is decimal-string in the protocol; `0` terminates. Your temporary map-based SCAN must still honor "cursor survives across calls while keys churn."
- Keep every error string in one `errors.go` — the harness will make you touch them repeatedly; one place beats twenty.

---

# Chapter 12 — The big picture

> **Time:** 2 h · **Before you start:** Chapter 11 — deliberately placed after your first build, because it is abstract before it.
>
> **You're done when:** You can draw the layer diagram and say what belongs in `beforeSleep` versus `serverCron`.


## 12.1 The process at rest

```
                    ┌────────────────────────────────────────────┐
                    │                redis-server                │
                    │                                            │
   clients ──TCP──▶ │  event loop (main thread)                  │
                    │  ┌──────────┐   ┌──────────────────────┐   │
   replicas ◀─TCP── │  │ ae loop  │──▶│ command execution    │   │
                    │  └──────────┘   │  dict ─ expires      │   │
   master ──TCP──▶  │       │        │  watched ─ blocked    │   │
   (if replica)     │       ▼        │  pubsub ─ backlog     │   │
                    │  serverCron    └──────────────────────┘   │
                    │       │                                    │
                    │  bio threads: fsync ─ close ─ lazyfree     │
                    │  fork children: BGSAVE ─ BGREWRITEAOF      │
                    └────────────────────────────────────────────┘
                         │                    │
                    appendonly.aof        dump.rdb
```

Three structs orbit everything (`server.h`) — internalize their fields and the whole codebase becomes navigable:

- **`struct redisServer`** (one global, `server`) — config, the db array, replication state (`master_replid`, `master_repl_offset`, `repl_backlog`, `slaves` list), AOF state (`aof_buf`, `aof_state`, child pid), stats, the loop pointer. Your Go equivalent is the engine struct.
- **`struct redisDb`** — `dict` (keyspace), `expires`, `blocking_keys`, `ready_keys`, `watched_keys`. Note which concerns live *per-db*.
- **`struct client`** — fd/conn, input buffer + parse resume state (`multibulklen`, `bulklen`), argv, output buffer, flags (`CLIENT_MULTI`, `CLIENT_BLOCKED`, `CLIENT_DIRTY_CAS`, `CLIENT_MASTER`, `CLIENT_SLAVE`…), MULTI queue, watched list, replication ack offset. **A replica is a client. The AOF loader is a client. A Lua call is a client.** The fake-client trick — one execution interface, many drivers — is the architecture's best idea and your ch. 7's `Client` struct should be designed for it from day one.

## 12.2 Boot, in order

`main` (server.c) → parse config → `initServer` (create loop, listeners, dbs, bio threads, register `serverCron` + connection acceptor) → `loadDataFromDisk` (AOF if enabled, else RDB — **before** accepting commands; serving `-LOADING` to early connections) → if replica config present, schedule the ch. 27 handshake → `aeMain`. ch. 7 mirrors this shape exactly; the ordering (load *before* serve) is a done-when.

## 12.3 The two heartbeats

**`serverCron`** — default 10/s (`hz`), the janitor: active expiry cycle (§9.3) · incremental-rehash step (§15.2) · save-point check against dirty counter (§20.2) · `replicationCron` (acks, timeouts, reconnect) (§27.5) · clientsCron (idle timeouts, output-buffer-limit enforcement, input-buffer sanity) · LRU clock refresh · stats rollups. Everything in it must be **bounded per tick** — cron is inside the loop; a slow cron is a slow server (the §9.3 time budget exists exactly for this).

**`beforeSleep`** — every loop iteration, the completer: flush AOF buffer (§20.3) · handle clients newly unblocked (§31.3) · stream replies/replica feed to sockets · fast expire cycle when backlogged. The split rule: **cron does periodic maintenance; beforeSleep completes work the just-finished iteration generated.** Getting a task on the wrong side is a real bug class: AOF flush in cron instead of beforeSleep = up to 100 ms of acknowledged-but-unwritten writes at hz=10.

## 12.4 Layering

```
transport (anet/connection)  →  protocol (networking: parse/reply)
    →  dispatch (server.c: lookup, arity, auth, OOM/loading gates, MULTI queueing)
        →  semantics (t_string, t_list, …: the commands)
            →  keyspace (db.c: lookup/add/delete + choke-point hooks)
                →  structures (dict, listpack, skiplist, quicklist, intset)
cross-cutting, fed by the choke point: propagation (AOF + replication) · notifications
    · watched/ready keys · dirty counter · stats
```

The dependency arrows only point down. `t_list.c` doesn't know AOF exists; `dict.c` doesn't know keys expire. When your ch. 19 code wants to call `feedAOF` from inside a list function, this diagram is the "no."

# Chapter 13 — Request lifecycles, traced

> **Time:** 2 h · **Before you start:** Chapter 12.
>
> **You're done when:** You can trace `SET k v EX 10` from socket bytes to disk bytes from memory.


Trace these by hand once each — they are the integration tests of your understanding. Each numbered step names the real function.

## 13.1 `SET k v EX 10`

1. Socket readable → `readQueryFromClient` appends to `c->querybuf`.
2. `processInputBuffer` → `processMultibulkBuffer` extracts `["SET","k","v","EX","10"]` into argv (resumable if partial).
3. `processCommand`: lookup in command table → arity ok → not OOM (or eviction runs, §24.1) → not in MULTI → `call()`.
4. `setCommand` → `setGenericCommand`: parse options; `tryObjectEncoding("v")`; `setKey(db,k,obj)` — choke point: dirty++, touch watchers, notification `set`; `setExpire(db,k,now+10000)`.
5. Propagation: rewritten to `SET k v PXAT <abs>` (§20.4) → `feedAppendOnlyFile` (to aof_buf) + `replicationFeedSlaves` (to backlog + replica buffers).
6. `addReply(c, shared.ok)` → output buffer.
7. …loop iteration ends → `beforeSleep`: aof_buf → write() (fsync per policy); output buffers → sockets. Client reads `+OK`.

Note what step 7 means under `everysec`: the client saw `+OK` before the disk did. That's the documented trade, visible right there in the ordering.

## 13.2 `GET k` on an expired key

1–3 as above → `getCommand` → `lookupKeyReadWithFlags` → `expireIfNeeded`: TTL past → **not a replica** → delete key; propagate `DEL k` to AOF+replicas; notification `expired`; touch watchers → lookup returns NULL → reply `$-1`. One read produced a write; every downstream consumer hears `DEL`, none of them heard "expired by clock."

On a **replica**: same until the branch — key is hidden (return NULL) but not deleted, no propagation; the authoritative `DEL` arrives on the replication stream when the master notices.

## 13.3 `BLPOP q 0` then, elsewhere, `RPUSH q job`

1. Client A: `blpopCommand` → list empty → `blockForKeys(A, [q])`: A flagged blocked, registered in `db->blocking_keys[q]`; **no reply sent**; loop moves on. A's connection goroutine (Go) just waits on its reply channel.
2. Client B: `RPUSH q job` executes fully — list created, dirty++, propagated as RPUSH — and `signalKeyAsReady(db, q)` appends to `server.ready_keys`.
3. After B's command (and any MULTI batch) completes: `handleClientsBlockedOnKeys`: q ready → oldest waiter A → pop `job`, reply to A, **propagate `LPOP q`**.
4. Replica's view: `RPUSH q job` then `LPOP q` — two writes, no blocking anywhere.

## 13.4 `WATCH stock` → `MULTI` → `DECR stock` → `EXEC`, with a race

1. A: WATCH registers (A, stock). A: GET stock → "3". A: MULTI → +OK. A: DECR stock → +QUEUED (not executed).
2. B: `SET stock 7` → choke point → `touchWatchedKey(stock)` → A flagged `CLIENT_DIRTY_CAS`.
3. A: EXEC → dirty flag → **null array**, queue discarded, nothing propagated. A's library retries the whole loop.
4. Without the race: EXEC runs DECR, propagates `MULTI; DECR stock; EXEC` as a unit.

## 13.5 Replica full sync (both sides)

1. R: connect → PING/REPLCONF×2 → `PSYNC ? -1`.
2. M: `syncCommand` → no usable backlog match → `+FULLRESYNC <replid> <offset0>`; start BGSAVE (or attach R to one in flight); R marked WAIT_BGSAVE; **every write from now also queues in R's buffer**.
3. M: child done → stream RDB bytes to R (length-prefixed, or diskless EOF-delimited).
4. R: flush own db → load RDB stream → apply queued writes → steady state: applying feed, `REPLCONF ACK <offset>` each second.
5. M: cron sees acks; `WAIT` queries satisfied; backlog ring keeps last N bytes for R's next hiccup.

## 13.6 Cluster write to the wrong node

1. Client → node X: `SET user:{42}:cart …` → `getNodeByQuery`: slot=CRC16("42")%16384 → owner is Y → `-MOVED 8000 <Y>`.
2. Client updates its slot map, replays against Y → +OK (Y propagates to *its* replicas as ch. 27).
3. If slot 8000 were mid-migration Y→Z and the key absent on Y: `-ASK 8000 <Z>` → client sends `ASKING` + retry to Z, map unchanged.

---
---

# PART IV — DATA STRUCTURES AND COLLECTIONS

The computer-science core: five structures built from scratch, then the four collection types on top of them.

---

# Chapter 14 — Memory

> **Time:** 3 h · **Before you start:** Chapter 13.
>
> **You're done when:** You can explain why Go cannot fork, and what that costs you in chapter 23.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Memory optimization" — `redis.io/topics/memory-optimization`; the encodings/thresholds section | 30 min |
> | `sds.h` header comment + the five `sdshdr` struct variants — how far Redis goes to save 3 bytes per string | 30 min |
> | `zmalloc.c`: `zmalloc_used_memory` — how used-memory accounting actually works (a counter, not a query) | 20 min |
> | **Evans, "A Scalable Concurrent malloc(3) Implementation for FreeBSD"** (BSDCan 2006) — the jemalloc paper. Redis links jemalloc *by default* precisely for the fragmentation behaviour §14.2 describes; size classes and arenas are why `used_memory_rss` diverges from `used_memory`. `people.freebsd.org/~jasone/jemalloc/bsdcan2006/jemalloc.pdf` | 1 h |

## 14.1 Why memory layout is the whole game

In a disk database, memory layout barely matters — disk dominates. In Redis, memory *is* the storage medium, so layout determines both capacity (how many keys fit) and speed (cache locality). Two numbers to hold:

- **A pointer costs 8 bytes, and a heap allocation costs ~16–48 bytes of overhead** (allocator header + size-class rounding). A linked list of 100 3-byte strings costs ~100 × (8 next + 8 prev + 8 value ptr + 48 alloc overhead + data) ≈ 7 KB for 300 bytes of payload. This single observation motivates listpack (§15.4), intset, SDS, and embstr.
- **A cache miss costs ~100 ns; a cache hit ~1 ns.** Chasing 5 pointers = 5 potential misses. A flat byte array you scan linearly prefetches beautifully. For small N, **O(N) linear scan of packed bytes beats O(1) pointer-chasing** — that's why small hashes/sets/zsets use packed encodings and convert only past a threshold (§8.2).

## 14.2 How C Redis counts every byte

`zmalloc` wraps `malloc` and keeps a global `used_memory` counter, incremented by the *allocator-rounded* size of every allocation. `maxmemory` enforcement (§24.1) compares this counter — an O(1) read — against the limit on every write command. `INFO memory`'s `used_memory` is this counter; `used_memory_rss` is what the OS reports; the ratio is **fragmentation** (allocated-but-unused slack inside the allocator). Redis ships an active defragmenter (`defrag.c`) — out of scope, but know why it exists.

## 14.3 The Go problem #1: you can't count bytes

Go gives you no per-allocation hook. Your options for `maxmemory` accounting, in order of usefulness:

1. **Logical accounting (build this).** Every stored object reports its approximate size via a `SizeOf()` you write: string = len + header constant; listpack = len(buf); dict = buckets × entry size + per-key/value sizes. Maintain a running counter updated on every Put/Del/mutation — the zmalloc pattern, one level up. Not exact; *consistently* inexact, which is all eviction needs.
2. `runtime.ReadMemStats` — stop-the-world-ish, coarse (includes GC slack, goroutine stacks, your replication buffers), and laggy. Use only as an `INFO` cross-check.
3. Cgo + manual allocation — defeats the purpose of the exercise.

Design consequence: **every value type you add in ch. 19 implements `SizeOf()` from day one.** Retrofitting accounting onto five data types is a week; carrying it along is free.

## 14.4 The Go problem #2: no fork, no copy-on-write

The most consequential C-vs-Go difference in this entire project.

C Redis snapshots (RDB, §20.2) by calling `fork()`. The child gets a **page-table copy** of the whole address space — a frozen, point-in-time view, at nearly zero upfront cost. Parent keeps serving writes; the kernel copies pages only when the parent modifies them (**copy-on-write**). The child serializes its frozen view at leisure.

Go cannot `fork()` — `syscall.ForkExec` only supports fork+exec (a fresh program), because a bare fork duplicates a process whose runtime expects its scheduler and GC threads to exist, and they don't survive the fork. So you must produce a point-in-time snapshot while serving writes, by construction rather than by kernel magic. Your realistic options, all covered in §23.3:

| Strategy | Cost | Fidelity |
|---|---|---|
| **Stop-the-world serialize** | pause = full serialize time (seconds per GB) | perfect |
| **Copy keyspace, serialize copy** | pause = O(keys) map copy (~100 ms per M keys), then 2× memory for shared values | perfect, if values are copy-on-write-by-discipline |
| **Copy-on-write by hand** (versioned store or shadow dict of pending changes) | complexity | perfect |
| **Fuzzy iteration** (serialize live under cursor, like SCAN) | cheap | *not* point-in-time — inconsistent snapshot; wrong for RDB semantics |

The recommended build: **copy-the-index + immutable-values discipline** — snapshot = shallow copy of the `map[string]*Obj` plus the rule that *mutations never modify a value in place; they replace the `*Obj`* (strings already work this way; collections need care — §23.3 handles each type). Then a background goroutine serializes the shallow copy while the engine keeps replacing pointers in the live map. This is persistent-data-structures thinking, and it is the single most instructive Go-specific design problem in the book.

## 14.5 GC pressure, briefly

Go's GC scans pointers. 100M `map[string]*Obj` entries = long mark phases and latency spikes. Real mitigations, in the order you should reach for them: fewer, bigger allocations (listpack does this for free — packed encodings are GC-friendly too); avoid pointers in hot structs (store small strings by value where possible); `GOGC`/`GOMEMLIMIT` tuning last. Don't optimize this preemptively — measure with `redis-benchmark` + `runtime/metrics` in ch. 38. But know *why* the packed encodings you build in chapter 17 help twice in Go: memory *and* GC.

## Self-check — Chapter 14

1. Why can a linear scan of a packed byte array beat a hash lookup for a 20-field hash?
2. What exactly does `used_memory` count in C Redis, and how will DiceMe maintain the equivalent?
3. Why does `fork()` give C Redis a free consistent snapshot? Which kernel mechanism pays for it, and when?
4. Why can't Go fork? What discipline makes "shallow-copy the map" a correct snapshot?
5. Name two ways packed encodings help a *Go* implementation that don't apply to C.

---

# Chapter 15 — The data structures you will build

> You build all five structures in chapter 17.
>
> **Time:** 12 h · **Before you start:** Chapter 14.
>
> **You're done when:** You can state SCAN's guarantee and why reverse-binary cursor order provides it.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `dict.c` top-of-file comment + `dictRehash`, `dictScan` (read the reverse-binary-iteration essay above `dictScan` — the best comment in the codebase) | 1 h |
> | `listpack.c` header comment — the exact byte format, worked through | 45 min |
> | Pugh, "Skip Lists: A Probabilistic Alternative to Balanced Trees" (CACM 1990) — `epaperpress.com/sortsearch/download/skiplist.pdf`; short, readable; then `t_zset.c:zslInsert` | 1 h |
> | `quicklist.h` struct comments; `intset.c` (tiny — read it all) | 30 min |
> | `rax.c` header comment — the compressed-radix-tree node format, worked through | 30 min |
> | **Aumasson & Bernstein, "SipHash: a fast short-input PRF"** (INDOCRYPT 2012) — `131002.net/siphash/siphash.pdf`. Redis's dict hashes with SipHash-1-2 (`siphash.c`), not a fast non-cryptographic hash, and §15.2 explains why: without a keyed PRF an attacker picks colliding keys and turns every O(1) lookup into O(n). Hash-flooding is the one security bug a from-scratch keyspace reliably ships with. | 45 min |

These are built as **standalone libraries with their own tests** in chapter 17, before ch. 19 wires them into commands. This chapter is the theory; ch. 17 is the spec.

## 15.1 SDS — and what replaces it in Go

C strings are null-terminated: O(N) length, no binary safety, buffer overruns. SDS (`sds.c`) prefixes a header {len, alloc} so length is O(1), contents are binary-safe, and append can grow geometrically. Five header sizes (`sdshdr5`…`sdshdr64`) exist to shave header bytes for short strings.

**In Go, `string` and `[]byte` already are SDS** — length-prefixed, binary-safe. You skip the build, but keep two SDS lessons: (a) preallocation policy — an append-heavy value should over-allocate (Go's `append` already doubles; fine); (b) the embstr idea — Redis stores strings ≤44 bytes inside the object header's cache line (one allocation, not two). Your `deduceTypeEncoding` already knows the 44; in Go the analogous trick is storing small strings by value in the object rather than behind a pointer — optional, and only if ch. 38's benchmark says so.

## 15.2 The dict: incremental rehash

The main keyspace is a chained hash table (`dict.c`). The one non-obvious mechanism: **rehashing never happens all at once.**

A dict holds **two** tables, `ht[0]` and `ht[1]`, plus `rehashidx`. When load factor crosses 1.0 (forced at 5.0), `ht[1]` is allocated at the next power of two and `rehashidx = 0` — that's the whole "start". From then on:

- Every dict operation first moves one bucket's chain from `ht[0]` to `ht[1]` (`_dictRehashStepIfNeeded`), advancing `rehashidx`.
- `serverCron` additionally moves up to 100 buckets per millisecond-slice (`dictRehash(d, 1000)` bounded by time) so an idle server still finishes.
- During rehash: **lookups check both tables; inserts go to `ht[1]` only** (so `ht[0]` monotonically drains). Delete checks both.
- When `ht[0]` empties: free it, swap, `rehashidx = -1`.

Why: rehashing 100M keys at once is seconds of stall (§1.4). Amortized, it's nanoseconds per op.

**Go decision:** Go's built-in map hides its growth (it *is* incrementally grown internally, but you can't observe or control it, can't iterate stably across growth, and can't implement SCAN's guarantee on it). **Build the dict.** It's ~300 lines, teaches the two-table pattern, and SCAN (§15.3) plus eviction sampling (§24.2) *require* access to buckets. Use Go's map only for internal bookkeeping (watchers, pubsub registries) where SCAN semantics don't apply. Sizing: power-of-two buckets, hash with `maphash` or xxhash, chain with slices or singly-linked entries.

## 15.3 SCAN and the reverse-binary cursor

`KEYS` is O(N) and blocks. `SCAN cursor` returns a page of keys plus the next cursor, doing bounded work per call. The guarantee: **every key present for the entire scan is returned at least once; keys added/removed mid-scan may or may not appear; duplicates are possible.** No guarantee is "no duplicates" — clients must tolerate them.

The problem: the table can *rehash between calls*. A naive 0,1,2,…,N bucket walk breaks — after a resize, visited/unvisited buckets interleave. The solution (`dictScan`): visit buckets in **reverse-binary-increment order** — add 1 to the cursor's *high* bit and carry *downward* (equivalently: reverse the bits, increment, reverse back). Because a table of size 2^n maps bucket `b` to buckets `b` and `b+2^n` when doubled (the hash's low bits index the table), reverse-binary order visits a bucket and *all its future split-images* before moving on. Result: a cursor from any table size remains valid in any other size, with only bounded re-visiting.

Read the long comment above `dictScan` until you can re-derive the argument. Then implement SCAN in ch. 11 with a small table, force a rehash mid-scan in a test, and verify the guarantee holds. This is a done-when.

## 15.4 listpack (and its ancestor ziplist)

A **listpack** is a small collection serialized into one contiguous byte buffer:

```
<total-bytes u32> <num-elements u16> <entry>* <0xFF end>
entry: <encoding+len header (1-5 bytes)> <payload> <backlen (1-5 bytes)>
```

Elements are strings or integers; integers are stored in the smallest fitting width (13-bit, 16, 24, 32, 64 — the encoding byte says which). `backlen` (the entry's own length, written at its *end*) enables backward traversal without offsets. Insert/delete = `memmove` of the tail + header updates: O(N), fine for N ≤ 128ish.

Why it exists: §14.1 — for small N, one allocation and linear scans beat a real structure's pointers, by ~10× memory and often speed. Lists, hashes, sets, and zsets *all* use a listpack while small (§8.2 thresholds), converting to the real structure when they outgrow it.

Ziplist (`ziplist.c`) is the deprecated ancestor — same idea, but entries encoded the *previous* entry's length at their head, so growing one entry could cascade re-encodings through the rest (`O(N²)` worst case, the infamous "cascade update"). Listpack's design (each entry self-describing, backlen at the tail) exists specifically to kill that. Build listpack only; read ziplist's cascade-update comment as a cautionary tale.

**Build in Go**: `type listpack []byte` with Insert/Delete/Get/Iterate — real byte-twiddling, ~500 lines with tests. This is the most "from scratch" artifact in the project; don't cheat with `[]string`, the whole point is the encoding.

## 15.5 quicklist

A **quicklist** is the production list: a doubly-linked list *of listpacks*, each node bounded by `list-max-listpack-size` — which defaults to `-2`, meaning **8 KB per node** rather than a fixed entry count (§8.2). Ends stay unpacked-ish for fast push/pop; middle nodes can be LZF-compressed (`list-compress-depth`) since list access concentrates at the ends. You get: O(1) amortized push/pop at both ends, bounded memmove costs (one node, not the whole list), decent memory.

Build: a plain doubly-linked list of your listpacks, split-on-overflow, merge-on-underflow (adjacent nodes both < half-full). Skip node compression — you write the LZF codec in ch. 23 for RDB anyway, so wiring `list-compress-depth` on top of it afterwards is an afternoon, and it changes nothing a client can observe.

## 15.6 skiplist

Sorted sets need: insert/delete by member, score lookup by member (that's a companion dict), **range queries by score and by rank**, all at O(log N). Redis uses a skiplist (`t_zset.c:zslInsert`), not a balanced tree, because — antirez's stated reasons — it's simpler to implement/debug, and range iteration is trivial (level-0 is a sorted linked list).

Mechanism: each node gets a random level (level i+1 with probability 0.25, cap 32); level i is a sorted linked list skipping ~4^i nodes; search descends levels. Two Redis twists: nodes carry **span** (how many level-0 nodes each forward pointer jumps) so `ZRANK` is O(log N) — implement spans, most tutorials skip them and then `ZRANK` becomes O(N); and level-0 has **backward pointers** for reverse ranges (`ZREVRANGE`).

Ordering rule: by score, then lexicographically by member for equal scores. The zset = skiplist + `dict member→score`; both point at the same member string. Every mutation touches both; a mismatch is corruption — assert consistency in tests.

## 15.7 intset

A set of only integers, stored as a **sorted array** with a uniform width (16/32/64-bit) that **upgrades** when a bigger int arrives (re-encode the whole array; never downgrades). Membership = binary search. ~150 lines in Go. It's the warm-up build — do it first in chapter 17.

## 15.8 rax (radix tree)

A **compressed radix tree** (`rax.c`): a trie whose single-child chains are collapsed into one node holding the whole shared substring, so a key like `stream-entry-1700000000000-0` costs a handful of nodes rather than thirty. Nodes are variable-length allocations with the child pointers packed after the characters; lookup walks characters, insert splits a compressed node when the shared prefix ends.

You need it for **streams** (ch. 41–42), which index entries by their 128-bit ID and need ordered iteration and range starts — exactly what a radix tree gives and a hash table does not. Redis also used one for cluster slot→key tracking and uses it for client tracking's prefix table (ch. 48).

Build the ordered-map subset: `Insert`, `Find`, `Remove`, `SeekGE`/`SeekLE`, forward and reverse iteration. Skipping the compression (a plain 256-way trie) works and costs an order of magnitude in memory on stream IDs, which is the whole reason the structure exists — so compress.

## 15.9 Geohash — how GEO commands are a zset in disguise

Redis's geospatial support (`GEOADD`, `GEOSEARCH`, `GEODIST`…) adds **no new storage type**. `GEOADD sf 13.361389 38.115556 "Palermo"` stores into an ordinary sorted set whose *score* is a **52-bit geohash**: latitude and longitude are each binary-subdivided 26 times (is it in the left or right half of the range? emit a bit, halve the range — `geohash.c:geohashEncode`), and the two bit-strings are **interleaved** (lng, lat, lng, lat…) into one integer. That interleaving is a **Z-order / Morton curve**: it maps 2-D space onto a 1-D line such that numerically-close scores are (usually) geographically-close cells — which is exactly what a zset can range-query.

Why 52 bits: an IEEE-754 double's mantissa holds 52 bits exactly, so the geohash survives as a zset score with zero loss. Precision at 26 subdivisions ≈ 0.6 m — plenty.

A radius/box query (`GEOSEARCH … BYRADIUS 200 km`) then works like this (`geohash_helper.c`):

1. Pick the deepest geohash cell size that still covers the radius (fewer bits = bigger cell).
2. Take the cell containing the center **plus its 8 neighbors** (the 3×3 block — needed because the center may sit near a cell edge, and because the Z-curve has seams where numeric adjacency lies about spatial adjacency).
3. Each of the 9 cells is a contiguous **score range** (the cell's bit-prefix padded with 0s … padded with 1s) → 9 zset range scans.
4. Filter candidates by *exact* haversine distance (the cells over-approximate).

The lesson generalizes: **map a query shape onto a structure you already have** — 2-D proximity became 9 range scans on a 1-D ordered index. The same trick (space-filling curves over B-trees) powers geo-indexes in MySQL, DynamoDB, and MongoDB. Build it in ch. 44: the encode/decode + neighbours math is ~300 lines and the rest is zset calls you already own.

> 📚 Wikipedia: "Geohash" and "Z-order curve" (`en.wikipedia.org/wiki/Geohash`, `en.wikipedia.org/wiki/Z-order_curve`) · G.M. Morton, *A Computer Oriented Geodetic Data Base* (1966) — the original · `geo.c:geoaddCommand`, `geohash.c:geohashEncode`, `geohash_helper.c` in the redis source.

## Self-check — Chapter 15

1. During incremental rehash, why do inserts go only to `ht[1]`? What invariant would double-inserting break?
2. State SCAN's exact guarantee. Why does bucket order 0,1,2,… break it under resize, and what property of reverse-binary order fixes it?
3. In listpack, why is the entry length written at the entry's *end* too? What did ziplist do instead, and what disaster followed?
4. Why a skiplist over a red-black tree, per antirez? What does the span field buy?
5. Why does intset upgrade but never downgrade?
6. For each structure — dict, listpack, quicklist, skiplist, intset, rax — name the command family that dies without it.

---
---

# Chapter 16 — Reading the source: the dict

> **Time:** 2 h · **Before you start:** Chapter 15.
>
> **You're done when:** You can re-derive the `dictScan` argument without the comment in front of you.


## dict (2 h)

Read: `dict.c`: `dictAddRaw`, `_dictRehashStepIfNeeded`, `dictRehash`, `dictResize` policy, then the `dictScan` comment-essay until you can re-derive it; skim `kvstore.c`'s header comment for the 8.x contrast (per-slot dicts).
What you steal: two-table rehash state machine; rehash-step placement (every op + cron slice); the reverse-binary cursor, verbatim.

# Chapter 17 — Build: the data-structure libraries

> Six standalone libraries with their own tests.
>
> **Time:** 55 h · **Before you start:** Chapters 14, 15, 16.
>
> **You're done when:** `go test ./core/ds/...` is green, the fuzzers run clean, and chapter 11's harness still diffs clean on the new dict.


**Goal**: six packages under `core/ds/`, each standalone, tested, benchmarked, and API-shaped for chapters 19 and 42. No server changes except swapping the keyspace onto your dict at the end. Order: intset (warm-up) → dict → listpack → skiplist → quicklist → rax.

## 17.1 `ds/intset` (~4 h)

`type IntSet struct{ enc uint8; buf []byte }` — widths 16/32/64, sorted, binary-search membership, upgrade-on-demand (§15.7). API: `Add(int64) (added bool)`, `Remove`, `Contains`, `Len`, `Get(i)`, `Random()`. Tests: property vs `map[int64]struct{}` model; upgrade at boundary values (int16 max+1 etc.); never-downgrade.

## 17.2 `ds/dict` (~10 h)

Per §15.2: two tables, power-of-two sizing, `rehashidx`, step-on-every-op + `RehashMillis(budget)` for cron; `Scan(cursor, fn)` reverse-binary; `RandomKey()` honest-random (bucket-then-chain); load-factor 1.0 grow (5.0 forced), 0.1 shrink. Hash: `maphash` seeded per-dict.
Tests: model-diff property test under random ops; **SCAN guarantee under forced rehash mid-scan** (the §15.3 done-when); rehash-completes-on-idle via budget calls; benchmark vs native map (expect ~2-3× slower — you're buying capabilities, not speed; *record the number honestly in the README*).
Then: swap `redisDb` onto it behind the ch. 11 API; re-run ch. 11's harness — **zero diffs** is the acceptance test; SCAN drops the snapshot hack.

## 17.3 `ds/listpack` (~15 h — budget it, it's fiddly)

Exact format of §15.4 (match Redis's byte format precisely — this pays off in ch. 23 when RDB dumps listpacks raw, and it makes `hexdump` comparisons against real Redis possible). API: `New`, `Insert(pos)`, `Append`, `Delete(pos)`, `Get(pos) (val []byte, isInt bool, i64 int64)`, `Len`, `Bytes`, iterators both directions, `Seek`.
Tests: round-trip property test (random op sequence vs `[]string` model); **fuzz the decoder** (corrupt bytes must error, never panic — this listpack gets loaded from RDB files later, i.e., from disk you can't trust); back-length correctness by walking backward over random content; every integer-width boundary.

## 17.4 `ds/skiplist` (~10 h)

Per §15.6: levels p=0.25 max 32, **spans**, backward pointers; score-then-member ordering (member compare = bytes). API: `Insert(score, member)`, `Delete`, `UpdateScore` (delete+reinsert unless position unchanged — Redis optimizes this; you can too), `FirstInRange/LastInRange(min, max, minEx, maxEx)`, `Rank(member’s score+name) int`, `GetByRank(n)`, `DeleteRangeByScore/Rank`.
Tests: model = sorted slice; property-test all ops; span-consistency invariant checked after every op in tests (`rank(GetByRank(i)) == i`); the p=0.25 level distribution sanity check.

## 17.5 `ds/quicklist` (~6 h)

Doubly-linked nodes wrapping your listpack; fill limit from config — implement **both** senses of `list-max-listpack-size` (positive = entry count, negative = the -1…-5 size ladder, default -2 = 8 KB), since real Redis's default is the size one; split/merge; API mirrors list commands' needs: PushHead/Tail, PopHead/Tail, `InsertBefore/After(pivot)`, `Index(n)`, `Range(from,to)`, `RemoveRange`, iterator with cursor stability across node boundaries.
Tests: model-diff vs `[]string`; node-count sanity under interleaved push/pop (no leak of empty nodes); merge threshold behavior.

## 17.6 `ds/rax` (~10 h)

Per §15.8: compressed radix tree over `[]byte` keys with arbitrary values. API: `Insert(key, val) (added bool)`, `Find(key)`, `Remove(key)`, `Len`, `SeekGE(key)` / `SeekLE(key)`, and iterators in both directions with cursor stability across the node splits an insert can cause mid-iteration. Node layout is yours (Go structs, not Redis's packed allocation) — the *compression* and the ordered iteration are the parts that matter.
Tests: model-diff against a sorted `[][]byte` under random insert/remove sequences; prefix-heavy key sets (a million stream-shaped IDs) with a memory comparison against an uncompressed trie recorded in the README; split-during-iteration correctness; fuzz the key bytes (binary-safe, including embedded zeros and the empty key).

## 17.7 Done-when

`go test ./core/ds/... -count=1` green; fuzzers clean for 5 min each; benchmarks recorded in `core/ds/README.md`; ch. 11 harness still zero-diff on the dict-backed store.

---

# Chapter 18 — Reading the source: the collections

> **Time:** 2.5 h · **Before you start:** Chapter 17.
>
> **You're done when:** You can name each collection's conversion trigger and its exact threshold.


## Collections (2.5 h)

Read: `sort.c:sortCommandGeneric` (the BY/GET pattern dereference); `t_list.c`: `pushGenericCommand`, `listTypeTryConversionRaw` (threshold logic); `quicklist.c`: `quicklistPush`, node split/merge; `listpack.c`: `lpInsert` + the format comment; `t_hash.c`: `hashTypeSet` + conversion; `t_set.c`: `setTypeAdd` (intset→listpack→dict ladder); `t_zset.c`: `zslInsert` (spans!), `zslDeleteNode`, `zsetAdd` (the flags matrix NX/XX/GT/LT/CH/INCR), `genericZrangebyscoreCommand`; `intset.c` whole.
What you steal: each conversion trigger, exactly; zslInsert with spans, near-verbatim; ZADD's flag semantics table.

# Chapter 19 — Build: the collections

> **Time:** 48 h · **Before you start:** Chapters 17 and 18.
>
> **You're done when:** `tests/ch19_commands.txt` diffs clean, and `OBJECT ENCODING` flips at exactly the configured thresholds.


**Goal**: lists, hashes, sets, sorted sets — full core command surface, correct encodings with conversions at exact thresholds. After this chapter DiceMe is a *useful database*.

## 19.1 The wiring pattern (same for all four types)

Each type gets `core/cmd/<type>.go` + an object layer owning the encoding switch:

```
listTypeGet(obj) — asserts TYPE, returns an interface over {listpack | quicklist}
listTypeTryConversion(obj) — after growth: entries > list-max-listpack-size,
                             or any element > threshold bytes → convert up, once
```

Conversion = build the big structure from the small one, replace `obj.Value` + encoding, **never in place while a snapshot is live** (§20.5 — the discipline starts now even though snapshots arrive in chapter 23; document it on every mutator).

Every write goes through store hooks (dirty, notify, signalReady — the last one matters here: lists feed BLPOP later).

## 19.2 Command surface (per type)

**Lists** (listpack→quicklist): LPUSH/RPUSH/LPUSHX/RPUSHX, LPOP/RPOP (+count), LLEN, LRANGE (negative indexes!), LINDEX, LSET, LINSERT BEFORE|AFTER, LREM (±count semantics), LTRIM, LMOVE/RPOPLPUSH, LPOS (RANK/COUNT/MAXLEN).
**Hashes** (listpack→dict): HSET(multi)/HSETNX, HGET/HMGET/HGETALL, HDEL, HLEN, HEXISTS, HKEYS/HVALS, HINCRBY/HINCRBYFLOAT, HSTRLEN, HRANDFIELD, HSCAN.
**Sets** (intset→listpack→dict): SADD/SREM, SISMEMBER/SMISMEMBER, SMEMBERS, SCARD, SPOP(+count — remember §20.4!), SRANDMEMBER (±count semantics differ — read the doc), SMOVE, SINTER/SUNION/SDIFF (+STORE, +SINTERCARD), SSCAN.
**Zsets** (listpack→skiplist+dict): ZADD (NX XX GT LT CH INCR — the full matrix from `zsetAdd`), ZREM, ZSCORE/ZMSCORE, ZCARD, ZINCRBY, ZRANK/ZREVRANK (WITHSCORE), ZRANGE (REV, BYSCORE, BYLEX, LIMIT — the unified 6.2 form + legacy ZRANGEBYSCORE/ZREVRANGEBYSCORE/ZRANGEBYLEX/ZREVRANGEBYLEX), ZRANGESTORE, ZCOUNT, ZLEXCOUNT, ZPOPMIN/ZPOPMAX, ZRANDMEMBER, ZREMRANGEBYRANK/SCORE/LEX, ZSCAN, and the set-algebra family **ZUNION/ZINTER/ZDIFF/ZINTERCARD + ZUNIONSTORE/ZINTERSTORE/ZDIFFSTORE** with their `WEIGHTS` and `AGGREGATE SUM|MIN|MAX` options — fiddly but mechanical, and every leaderboard library sends them.
**Multi-key pops** (7.0 forms, once lists+zsets exist): LMPOP/ZMPOP — first non-empty of N keys; their blocking twins land in ch. 33.
**`SORT`/`SORT_RO`** (`sort.c:sortCommandGeneric`): `BY pattern` / `GET pattern` dereferencing (`weight_*`, `GET obj_*->field`, the literal `#`), `ALPHA`, `LIMIT`, `STORE`, and the sorted-set/set/list input rules. It is the closest thing Redis has to a join, the pattern-dereference parser is a genuinely good exercise, and `SORT` with `BY` is *not* deterministic across nodes — which is why it propagates as its effect (§20.4) when it has a `STORE`. Required; budget 6 h of the 48.

Hash **field TTLs** (7.4's `HEXPIRE` family) are deliberately deferred to ch. 48: they need per-field metadata and a second expiry index, and bolting that onto the hash object layer now would complicate every other hash command for a feature you cannot test properly until expiry, propagation and persistence are all finished. Leave a comment in the hash object layer saying so.

Score parsing: `+inf`/`-inf`/`(3.5` exclusive ranges; lex ranges `[a (b - +`. Diff-harness these hard — range-boundary bugs are this chapter's signature failure.

`SizeOf()` on every representation lands **now** (§14.3), even though eviction only reads it in chapter 26.

## 19.3 Done-when

```bash
tests/harness/diff.sh tests/ch19_commands.txt        # the big one: ~1000 lines, all four types, zero diffs
# encoding flips at exact thresholds:
redis-cli -p 7379 CONFIG SET hash-max-listpack-entries 3
#   HSET 3 fields → listpack ; 4th field → hashtable ; never back
# 1M-member zset: ZADD in pipeline; ZRANGEBYSCORE latency sane; ZRANK O(log n) sane (time it)
# storm/mixed: interleaved type ops, 10 min, RSS stable, zero errors
```

## 19.4 Traps

- LREM/LINSERT/LPOS on quicklist cross node boundaries — the iterator tests from 20.5 earn their keep.
- Empty-collection rule: **a collection that becomes empty is deleted** (`HDEL` last field → key gone, `EXISTS` → 0). Every pop/rem command needs this; forgetting it diverges DBSIZE/EXISTS/TYPE everywhere (the harness will scream).
- SINTERCARD/SRANDMEMBER negative-count semantics: read the command docs *while writing the tests, before the code*.
- ZADD GT/LT + NX combinations are errors; the matrix in `zsetAdd` is the truth table — port it, don't re-derive it.
- Blocked on a range-boundary diff for an hour → you're inclusive/exclusive-swapped on one end; check `(` handling first.

---

---
---

# PART V — PERSISTENCE AND EVICTION

Making memory survive a crash, and deciding what dies when memory runs out.

---

# Chapter 20 — Persistence: RDB and AOF

> **Time:** 4 h · **Before you start:** Chapter 19.
>
> **You're done when:** You can fill in the crash matrix in §20.6 from memory.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Persistence" — `redis.io/topics/persistence`; the best overview antirez ever wrote, largely still his text | 45 min |
> | `rdb.c`: `rdbSaveRio` (the writer loop) + the opcode defines in `rdb.h` | 1 h |
> | `aof.c`: file comment (the multi-part AOF design doc), `feedAppendOnlyFile`, `flushAppendOnlyFile` | 1 h |
> | ch. 21 of this book before building; `tests/integration/aof.tcl` | 45 min |
> | **Rosenblum & Ousterhout, "The Design and Implementation of a Log-Structured File System"** (SOSP 1991) — `web.stanford.edu/~ouster/cgi-bin/papers/lfs.pdf`. Your AOF *is* a log-structured store and `BGREWRITEAOF` *is* segment cleaning; this is where that idea is stated properly, along with the write-amplification arithmetic that decides your rewrite threshold. | 1.5 h |

## 20.1 Two philosophies

| | RDB | AOF |
|---|---|---|
| What's on disk | point-in-time binary snapshot | log of every write command |
| Recovery | load snapshot | replay log |
| Loss window | since last snapshot (minutes) | per fsync policy (0–1 s) |
| File size / load time | compact / fast | large / slow (until rewrite) |
| Runtime cost | fork spike, COW memory | small per-write CPU + buffer |

Production commonly runs **both**; on boot, **AOF wins** if enabled (it's more complete). Modern default AOF is *RDB-preamble*: the rewritten base is an RDB file, with RESP commands appended after — you get RDB's load speed and AOF's completeness. You will build: RDB fully, AOF fully, rewrite with RDB preamble as the final step.

## 20.2 RDB

**Format** (build your own, one byte at a time — this is the point):

```
"REDIS0011"                       magic + version
0xFA <key> <val>                  AUX: redis-ver, redis-bits, ctime, used-mem, repl-id, repl-offset
0xF5 <payload>                    FUNCTION2: a serialized function library (ch. 40)
0xFE <dbnum>                      SELECTDB
0xFB <dict-size> <expires-size>   RESIZEDB hint
per key:
  [0xFD <s-timestamp u32le>]      optional EXPIRETIME     (seconds, legacy)
  [0xFC <ms-timestamp u64le>]     optional EXPIRETIME_MS
  [0xF8 <idle len-encoded>]       optional IDLE  (LRU, written with maxmemory-policy lru)
  [0xF9 <freq u8>]                optional FREQ  (LFU, written with maxmemory-policy lfu)
  <value-type byte>               0=string 1=list… (one per type × encoding)
  <key: string-encoded> <value: type-specific encoding>
0xFF                              EOF
<CRC64 u64le>                     checksum of everything before
```

Two details that decide whether your files and real Redis's files are mutually readable:

**The string micro-format** (`rdbSaveLen` / `rdbSaveStringObject`): the top two bits of the first byte select 6-bit, 14-bit, or 32/64-bit lengths, and the fourth combination (`0b11`) selects a *special* encoding — int8/int16/int32 stored as an integer, or **LZF-compressed**. Implement all of them, **including LZF**. It is tempting to skip compression and refuse compressed files, and it is exactly the shortcut that makes "load a real `dump.rdb`" impossible: real Redis compresses any string longer than 20 bytes when `rdbcompression yes`, which is the default, so essentially every production RDB file in existence contains LZF blocks. LZF itself is about 150 lines for the decompressor and another 100 for the compressor — the smallest compression algorithm you will ever implement, and the whole of ch. 23's interop requirement rests on it.

**The value-type bytes are per type *and* encoding.** A hash is type 4 when written as a dict and type 16 (`RDB_TYPE_HASH_LISTPACK`) when written as a listpack; a zset has both `RDB_TYPE_ZSET_2` and `RDB_TYPE_ZSET_LISTPACK`; lists are written as `RDB_TYPE_LIST_QUICKLIST_2` (a list of listpack blobs); streams as `RDB_TYPE_STREAM_LISTPACKS_3`. The full list is `rdb.h`'s `RDB_TYPE_*` defines — copy it wholesale into a Go const block, implement the writers for the encodings you produce, and implement the **readers for all of them**, because a real Redis file may use an encoding your server would not have chosen.

Collections serialize either as element streams or — key insight — **packed encodings dump their raw bytes**: a listpack-encoded hash is written as one string (the listpack buffer itself). Loading re-validates and possibly re-converts. This makes RDB fast *and* teaches why the encodings being self-contained byte arrays pays off twice.

**When RDB happens:** explicit `SAVE` (blocking — build it first), `BGSAVE`, automatic save points (`save 900 1 / 300 10 / 60 10000` — "N changes in M seconds", checked in serverCron against the **dirty counter**), and on `SHUTDOWN`. The dirty counter increments on every effective write — wire it into the §8.3 choke point now.

**The fork trick** (C): `BGSAVE` forks; the child serializes its frozen COW view; the parent tracks `child_pid`, refuses concurrent children, and reports progress via a pipe (`childinfo.c`). COW cost: every parent-side page write during the save duplicates a 4 KB page — worst case doubles memory. This is why "why did my Redis OOM during BGSAVE at 60% memory" is a canonical ops interview question.

**The Go build** (§14.4, spec in §23.3): snapshot = shallow map copy + replace-don't-mutate discipline; serializer runs on a goroutine over the frozen copy. Same observable semantics: point-in-time, non-blocking (except the bounded copy pause), one at a time.

## 20.3 AOF

Three moving parts, in write order:

1. **feed** — after an effective write executes, append its *propagated form* (§20.4) as a RESP array to an in-memory `aof_buf`. Not the socket bytes the client sent — the canonical rewritten command.
2. **flush** — in `beforeSleep` (once per loop iteration, batching all commands of that iteration), `write()` `aof_buf` to the file. One write per iteration, not per command.
3. **fsync** — per `appendfsync` policy:
   - `always`: fsync in the flush, before replies are sent. Loss window: 0 acknowledged writes. Cost: ~1 ms per iteration — throughput dies; ~200× slower. Correctness note most implementations miss: **the reply must not reach the client before the fsync covering its write completes** — otherwise "always" is a lie.
   - `everysec` (default): fsync on a **background thread** (`bio.c`) at most once per second. Loss window ≤ ~1 s. Extra rule: if the previous background fsync is still running (slow disk), the flush *stalls the write()* rather than queueing unboundedly — backpressure, not memory growth.
   - `no`: let the OS decide (~30 s). Fastest, largest window.
4. **load** — on boot, if AOF enabled: read the file, parse RESP commands, execute them through a **fake client** (a client struct not attached to a socket — build this concept; it returns in replication §10 and scripting). Handle the torn tail: a crash mid-write leaves a truncated final command; `aof-load-truncated yes` (default) logs and drops the tail rather than refusing to start. Your ch. 23 tests kill -9 mid-write and assert exactly this.

**Rewrite** (`BGREWRITEAOF` — the real one): the log grows forever (`INCR x` × 1M = 1M lines for one key). Rewrite generates the *minimal* log: serialize current state (as RDB preamble, or as SET/RPUSH/HSET commands — build commands first, preamble later). While rewriting from a snapshot, **new writes keep appending to the live AOF and are also buffered**; on completion, swap files atomically (temp + rename, §21.2) and splice the buffered tail. C Redis ≥7 does this with a **manifest**: `appendonlydir/` holding a base file + incremental files + a manifest listing them; the swap becomes "write new base + new incr + new manifest" with no giant splice. Build the single-file version; read `aof.c`'s comment to see why the manifest exists (the old splice had a corruption window).

Your audit-#4 file is a rewrite-only AOF opened with `O_APPEND` and no load: after this chapter you can name each of its five sins precisely.

## 20.4 Propagation: effects, not intentions

What goes into the AOF (and to replicas — same stream) is not always what the client typed:

| Client sent | Propagated |
|---|---|
| `SET k v EX 10` | `SET k v PXAT <absolute-ms>` — replay much later must not extend the TTL |
| `EXPIRE k 10` | `PEXPIREAT k <absolute-ms>` |
| `INCRBYFLOAT k 1.1` | `SET k <result>` — float arithmetic must not re-run (platform drift) |
| `SPOP k` | `SREM k <the-popped-member>` — randomness must not re-run |
| GET-path expiry | explicit `DEL`/`UNLINK` (§9.4) |
| `LPUSH` (nothing happened, wrong type) | nothing — only *effective* writes propagate |

The principle: **the log records deterministic effects.** Randomness, clocks, and floats are resolved once, on the master, at execution time. `alsoPropagate`/`propagateNow` (server.c) is the C mechanism for commands that propagate something other than themselves. Design your command signature for this from ch. 11: an eval returns `(reply, effect)` where effect defaults to "propagate verbatim" but can be overridden — retrofitting this in ch. 30 hurts.

## 20.5 Snapshot-without-fork: the Go design problem

Worked options (recommendation: **B**):

- **A. Stop-the-world.** Engine pauses, serialize everything, resume. Simple, correct, seconds of stall per GB. Keep as `SAVE` (which is *supposed* to block); unacceptable for `BGSAVE`.
- **B. Copy-the-index + immutable values.** Pause only to shallow-copy the keyspace map (and expires map) — O(keys) pointer copies, ~100 ms per million keys, tunable by copying incrementally per-dict-bucket. Discipline: **no in-place value mutation after snapshot** — strings already comply (replace `*Obj`); collections comply if mutations copy-on-write the specific node/buffer being changed *while a snapshot is live* (a global `snapshotActive` flag checked by mutators; listpacks copy the buffer, dict-based values copy the entry chain or the whole small dict). Serializer walks the frozen index at leisure.
- **C. Full versioning/MVCC.** Every mutation writes a new version; snapshots pin a version. Strictly more general (gives you consistent SCAN too), strictly more work. This is the ConsulMe/immutable-radix design — you'll appreciate the contrast.
- **D. Subprocess with pipe.** Serialize by streaming *commands* to a helper process that writes the file — doesn't solve consistency, just offloads I/O. Not sufficient alone.

B's trap to test for: a snapshot is live, a client runs `LPUSH biglist x` — assert the serializer sees the pre-push list. Write that test before the implementation; it fails against naive in-place push, and that failure is the lesson.

## 20.6 The crash matrix

Build the table, then make tests enforce every row (ch. 23 done-when):

| Config | `kill -9` at T | After restart |
|---|---|---|
| RDB only, last save T-60s | | everything since T-60s gone |
| AOF everysec | | ≤1 s of acknowledged writes gone; torn tail dropped cleanly |
| AOF always | | zero *acknowledged* writes gone (reply-after-fsync!) |
| AOF + rewrite in flight | | old AOF intact — rewrite must be invisible until the atomic rename |
| RDB + AOF both on | | AOF loaded, RDB ignored |

## Self-check — Chapter 20

1. Why does AOF flush once per event-loop iteration instead of per command? What batches?
2. State the loss window and the mechanism for each fsync policy. What extra ordering rule makes `always` honest?
3. Why is `SPOP` propagated as `SREM`? Give a divergence scenario if it weren't.
4. Why does BGSAVE double memory in the worst case in C? What's the analogous cost of strategy B in Go?
5. Walk the rewrite: where do concurrent writes go, and what makes the swap safe at every instant?
6. Boot with both RDB and AOF present: which loads, and why that one?

---

# Chapter 21 — Crash safety: the filesystem contract

> **Time:** 2 h · **Before you start:** Chapter 20.
>
> **You're done when:** You can write the temp-file/fsync/rename/fsync-dir sequence from memory and say why each step is there.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | **Pillai et al., "All File Systems Are Not Created Equal: On the Complexity of Crafting Crash-Consistent Applications"** (OSDI 2014) — `usenix.org/system/files/conference/osdi14/osdi14-paper-pillai.pdf`. The paper behind §21.2's liturgy: which orderings and atomicity properties you may actually assume, and how they differ per filesystem. It finds these bugs in SQLite, Git and PostgreSQL, so assume they are in yours too. | 1.5 h |
> | **Rebello et al., "Can Applications Recover from fsync Failures?"** (USENIX ATC 2020) — `usenix.org/conference/atc20/presentation/rebello`. On Linux a failed `fsync` may mark the pages clean anyway, so a *retry returns success while the data is gone* — the "fsyncgate" that bit PostgreSQL in 2018. The 1 ms cost in §21.1 is not the only thing worth knowing about fsync. | 1 h |

## 21.1 What fsync buys, exactly

`write()` → page cache (RAM). Power loss eats it. `fsync(fd)` → blocks until the device confirms durable storage of that file's dirty pages (modulo lying consumer drives — note and move on). Order matters: data-fsync **before** any metadata/rename that makes readers trust the data. Cost: ~1 ms SSD (the §1.1 number shaping all of §20.3).

## 21.2 Atomic replacement, the full liturgy

```
write temp file (same directory!)  →  fsync(temp)  →  rename(temp, final)  →  fsync(directory)
```

- rename is atomic *in namespace*: readers see old or new, never mixed — but only if the new bytes were fsynced first (else: new name, garbage content after crash).
- fsync the **directory** or the rename itself may not survive (`open(dir, O_RDONLY)` + `Sync()` in Go).
- Same filesystem required (rename doesn't cross mounts) — hence temp-in-same-dir, not `/tmp` (also why your `os.CreateTemp(dir, …)` takes the dir argument).
- This liturgy is RDB save, AOF rewrite swap, and cluster/nodes.conf save. Write it once (`persist/atomicfile.go`), test it with the kill-loop, reuse thrice.

## 21.3 Torn writes & checksums

A crash can persist a *prefix* of your last write (and rarely, with some filesystems/devices, interleavings — assume prefix for AOF-with-fsync discipline). AOF: tail may be half a command → truncate-to-last-valid-command loader (§20.3). RDB: half a file with valid header → the trailing CRC64 catches it (that's *why* it's trailing) → refuse to load, fall back per config. Test rig: a proxy-writer that duplicates every AOF write to N copies truncated at random byte offsets; loader must handle all N.

## 21.4 What to test (already in ch. 23's done-when — this is the why)

kill -9 loops at randomized points; power-loss simulation = SIGKILL is honest enough for process-level (device-level lies are out of scope, note them); the always-policy zero-loss proof; the rewrite-race loop; the torn-tail rig.

---

# Chapter 22 — Reading the source: RDB and AOF

> **Time:** 4 h · **Before you start:** Chapter 21.
>
> **You're done when:** You can describe the AOF everysec stall rule and the RDB child lifecycle.


## RDB (2 h)

Read: `rdb.c`: `rdbSaveRio` (the main loop), `rdbSaveLen`/`rdbSaveStringObject` (the length micro-format), `rdbSaveObject` for string+hash+zset cases (see packed-encodings-dump-raw), `rdbLoadRio` mirror, opcode list in `rdb.h`; `rdbSaveBackground` + `backgroundSaveDoneHandler` for child lifecycle.
What you steal: your file format (§20.2), byte for byte where convenient; the save-point/dirty logic; child-lifecycle states → your snapshot-goroutine states.

## AOF (2 h)

Read: `aof.c`: the file-header design comment (manifest rationale), `feedAppendOnlyFile`, `flushAppendOnlyFile` (**the everysec stall branch** — find it, it's the subtlest correctness code in the file), `rewriteAppendOnlyFileBackground`, `loadSingleAppendOnlyFile` (fake client!), `aof-load-truncated` handling.
What you steal: flush/fsync policy machine; fake-client loader; rewrite temp-file+rename choreography; torn-tail handling.

# Chapter 23 — Build: persistence

> The hardest engineering problem in this book, because Go has no fork.
>
> **Time:** 53 h · **Before you start:** Chapters 20, 21, 22.
>
> **You're done when:** Every row of the §20.6 crash matrix passes under repeated `kill -9`, including zero acknowledged loss under `appendfsync always`.


**Goal**: RDB save/load, AOF with all three fsync policies, background rewrite, correct boot loading, and the §20.6 crash matrix enforced by tests. Retires audit finding 4. Read ch. 21 first.

## 23.1 Order of work

1. `SAVE` (blocking, simple serializer) + `rdbLoad` at boot — the format, debugged in peace.
2. The snapshot mechanism (§23.3) → `BGSAVE` + save points.
3. AOF feed/flush/fsync + load — durability.
4. `BGREWRITEAOF` on the snapshot mechanism — compaction.
5. Crash matrix under storm.

## 23.2 RDB build notes

Format from §20.2. Write into a `bufio.Writer` wrapped in a CRC64-computing writer (implement the Jones polynomial per `crc64.c`, or vendor the table — it's a constant). Value serializers per encoding, packed-encodings-dump-raw where your listpack is byte-compatible (it is, if ch. 17 matched the format — reward collected). Loader: strict, but tolerant of *newer* value types by erroring with names, and validating listpacks via the fuzz-hardened decoder before trusting them (§17.3's "disk you can't trust").

**LZF, both directions** (~4 h). Decompression first — it is a simple back-reference format and it is what lets you read real files. Then compression, gated by `rdbcompression` (default yes) and applied to strings over 20 bytes, because your files should look like Redis's. Property-test round-trip on random and adversarial inputs (all-same-byte, no-repeats, exactly-at-the-window-boundary).

**`DUMP` and `RESTORE`** land here too, not in ch. 36 where they are first *used*: `DUMP` is the RDB value serializer plus a 2-byte RDB version footer and a CRC64; `RESTORE key ttl payload [REPLACE] [ABSTTL] [IDLETIME n] [FREQ n]` is the inverse with validation. Building them alongside the serializer costs an hour; building them later means touching the serializer twice. Cluster's `MIGRATE` (ch. 36) is then pure plumbing over commands that already exist.

Boot order per §12.2: load before serving; big files → `-LOADING Redis is loading the dataset in memory` for early connections (test it with a deliberate slow-load debug flag).

Save points: `save 900 1` config parsing; cron checks dirty counter + elapsed; `SHUTDOWN` saves if any save point configured (and `SHUTDOWN NOSAVE` doesn't).

**Interop is a requirement, not a stretch.** Your `dump.rdb` must load in real `redis-server`, and a real `dump.rdb` must load in DiceMe — including one written by a server with `rdbcompression yes` and a mixed workload of all five types. This is the single highest-value done-when in the chapter: it forces byte-exactness through the length encoding, the string encoding, the per-encoding value writers and the checksum all at once, and it turns `redis-check-rdb` into a free validator for the rest of the project. Budget the +6 h inside the 53.

## 23.3 The snapshot mechanism (§20.5 option B, specified)

```go
type Snapshot struct{ keys []kvPair; expires map[string]int64 }   // frozen view

engine, on BGSAVE request:
  if snapshotActive → -ERR Background save already in progress
  snap := store.ShallowCopy()      // O(keys); the only pause; measure it
  snapshotActive = true
  go func() { rdbSaveTo(tempfile, snap); rename; engine.notify(done) }()
```

The discipline, per type — write it as a table in code comments:

| Type/encoding | Mutation while snapshotActive |
|---|---|
| string (all) | already copy-on-replace (new *Obj) — free |
| listpack (any type) | copy buffer, mutate copy, swap pointer — cheap, bounded |
| quicklist | copy the *node* being touched + relink (persistent-list trick); or copy whole list if lazy — start lazy, optimize if measured |
| dict-encoded hash/set | clone-on-first-write-per-snapshot of the inner dict (flag on the Obj); coarse but simple |
| skiplist zset | same coarse clone-on-first-write; note the cost honestly |

The test you wrote before implementing (§20.5): snapshot live → mutate every type → serializer sees pre-mutation values. Plus: memory during snapshot bounded (RSS < 2× dataset for a 100%-write-touch workload — the honest COW-equivalent worst case).

`ShallowCopy` pause: measure with 1M keys, record it. If >200 ms, incrementalize per-bucket (the dict makes that possible — another 20.2 reward) — but only if measured.

## 23.4 AOF build notes

Per §20.3–9.4. The engine owns `aofBuf`; the propagation hook (declared in ch. 11, filled now) appends the **effect** form. Flush in the engine's per-iteration tail (your beforeSleep point — after draining a batch of requests, before selecting again). fsync: `always` inline *before* replies release (the honesty rule — restructure reply-release to after-flush for this mode); `everysec` on a dedicated goroutine (your bio thread), with the **stall rule**: if the previous fsync is still running, the *flush* blocks — bounded memory beats bounded latency here, same trade Redis makes.

Rewrites via §23.3 snapshots: serialize minimal commands (or RDB-preamble: emit your RDB bytes, then `*`-commands after — do commands first, preamble as the final step). During rewrite, engine appends live writes to both the real AOF and a rewrite-tail buffer; on child-done: temp file + tail splice + `rename` + fsync dir (§21.2), swap fds, bio-close the old one (§2.1's "why close is on the list" — now you do it too).

Loader: fake client from ch. 7 pays off — read RESP arrays, dispatch through the normal table with propagation *disabled* (a replay must not re-feed the AOF) — add the `CLIENT_REPLAY`-style flag to dispatch now; replication reuses it in ch. 30. Torn tail: `aof-load-truncated yes` behavior + test.

`appendfsync` switching at runtime via CONFIG SET — support, it's a config-registry validator away. `WAITAOF` needs replication acks and arrives in ch. 48; leave the fsync-offset counter it will read in place now (one integer, incremented per completed fsync), because retrofitting the counter later means auditing every flush path again.

## 23.5 Done-when

```bash
# the crash matrix, mechanized (each row = a script):
tests/ch23_crash.sh rdb-only      # kill -9 under storm; restart; data == last save point
tests/ch23_crash.sh aof-everysec  # loss window ≤ 1s of acks (storm records acked writes w/ timestamps)
tests/ch23_crash.sh aof-always    # ZERO acked writes lost (the hard one — proves reply-after-fsync)
tests/ch23_crash.sh rewrite-race  # kill -9 DURING BGREWRITEAOF x20 → always boots, never corrupt
redis-cli -p 7379 BGSAVE            # under storm: no client sees >200ms latency (measure!)
# snapshot isolation: the §23.3 test, all five type rows
# interop (required): dump.rdb loads in real redis-server && real dump.rdb (rdbcompression yes,
#   all five types, TTLs, 16 dbs) loads in dice with identical DEBUG DIGEST
redis-check-rdb dump.rdb ; redis-check-aof appendonly.aof   # real Redis's validators accept your files
redis-cli -p 7379 DEBUG RELOAD      # save+load round trip preserves everything, every type
```

## 23.6 Traps

- fsync ≠ write. `File.Sync()` is the fsync. The matrix rows fail without it and pass with it — run the always-row against a version without Sync once, watch it lose data, keep the log as a trophy.
- Rename without fsyncing the *directory* can lose the rename itself (§21.2).
- The everysec ack-recording storm client must record what the *server acked*, not what it sent — the difference is the whole measurement.
- AOF replay must run with active-expiry off and clock-independent (that's why §20.4 rewrote to PEXPIREAT). A key that expired between crash and restart: replay SETs it, replay PEXPIREATs it into the past, first lookup deletes it. Test that sequence.
- Go's `os.File.Write` is already unbuffered — your `bufio` layer is where "written but not flushed" hides. Audit every path for flush-before-fsync.

---

# Chapter 24 — Eviction

> **Time:** 2 h · **Before you start:** Chapter 23.
>
> **You're done when:** You can explain the eviction pool's insertion rule and how an 8-bit counter represents a million hits.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `evict.c`: file comment, `evictionPoolPopulate`, `performEvictions` — compare against your `core/eviction*.go` as you read | 1 h |
> | antirez, "Random notes on improving the Redis LRU algorithm" — `antirez.com`; the sampled-LRU design writeup, with the famous scatter plots. Docs companion: `redis.io/topics/lru-cache` | 30 min |
> | **Yang, Yue, Rashmi, "A large-scale analysis of hundreds of in-memory key-value cache clusters at Twitter"** (OSDI 2020) — `usenix.org/conference/osdi20/presentation/yang`. The best empirical answer to "does eviction policy actually matter?" Findings that will change what you build: most workloads are *not* Zipfian the way folklore says, TTL-driven expiry removes more objects than eviction does, and FIFO is often within noise of LRU. Read it before you spend a week tuning §24.3. | 1.5 h |

## 24.1 When eviction runs

Before executing any command that may allocate, `processCommand` checks: `used_memory > maxmemory`? Then `performEvictions` frees until under the limit (or gives up). If the policy is `noeviction` (default!) or nothing is evictable, **write commands are rejected** with `-OOM command not allowed when used memory > 'maxmemory'`; reads still work.

Get the trigger model right: eviction is **on the write path, synchronous, just-enough** — not a background sweeper, not a 40% purge (audit #12). It frees the *minimum* to fit the current need, so a workload hovering at the limit evicts one key at a time forever, which is exactly right.

## 24.2 The eight policies

```
noeviction                      reject writes when full (default)
allkeys-lru / volatile-lru      approximate LRU over all keys / only TTL'd keys
allkeys-lfu / volatile-lfu      approximate LFU
allkeys-random / volatile-random
volatile-ttl                    evict nearest-expiry first
```

`volatile-*` sample from the **expires dict**; if it's empty, volatile policies behave like noeviction. That's the entire difference — same machinery, different sampling source. (Your code samples only `store` — ch. 26.)

## 24.3 Approximate LRU: sample + pool

True LRU needs a linked list threaded through every object — 16 bytes/key and a list update per read. Redis instead:

- Stamps each object's 24-bit `lru` field with the **server-cached clock** (`server.lruclock`, refreshed by cron — reading the time syscall per lookup is too slow; your `getCurrentClock` calls `time.Now()` per access, fine at your scale, cache it when you care).
- On eviction: **sample `maxmemory-samples` (default 5) random keys** from the dict, compute idle time (with wraparound — your `getIdleTime` is already correct), and evict the idlest.

Sampling 5 approximates LRU badly per-decision but well in aggregate; antirez's plots show sample-10 nearly indistinguishable from true LRU. The **eviction pool** (`evictionPoolPopulate`) sharpens it further: a 16-slot array, kept sorted ascending by idle time, that *persists across evictions*. Each round's samples are inserted if idler than something present; eviction takes the idlest end. The pool accumulates the best candidates seen across rounds, so later evictions choose from better-than-random memory.

Pool insertion rules (this is exactly the code your audit-#3 bug is in): find rank by idle time; if the pool is full and the newcomer is less idle than everything, skip it; otherwise insert in position, dropping the **least-idle** occupant. Your version drops the *most*-idle (the best candidate) and corrupts the ordering — ch. 26 rewrites `Push` against property tests.

**Random-sampling a Go map vs the dict:** `for k := range m { break }` is *not* uniformly random and famously biased. Your own dict (§15.2) gives you honest random sampling: pick a random bucket, walk it. Another reason the dict build isn't optional.

## 24.4 LFU: a 4-bit-feel counter in 8 bits

`allkeys-lfu` reinterprets the 24-bit field: 16 bits = last-decay time (minutes), 8 bits = a **logarithmic counter** (Morris counter): on access, increment with probability `1/(counter × lfu_log_factor + 1)` — so 255 represents millions of hits. Decay: counter halves (or -1, config `lfu-decay-time`) per elapsed minute-window, so old fame fades. Eviction: same pool machinery, ranked by counter instead of idle time.

LFU beats LRU when "recently touched once" ≠ "hot" (scans pollute LRU). Build it in ch. 26 — it's ~40 lines once LRU works, and the probabilistic counter is a beautiful trick to own.

## 24.5 What eviction must also do

Each evicted key is a **write**: propagate `DEL`/`UNLINK` to replicas and AOF (replicas must not evict independently — same argument as §9.4, and yes, replicas *don't* run eviction on their own dataset for exactly that reason), fire `evicted` notification, count `evicted_keys` in INFO. And under the engine-goroutine model, eviction is naturally atomic with the write that triggered it — no one observes the over-limit state.

## Self-check — Chapter 24

1. Why is eviction on the write path rather than a background job? What does "just enough" prevent?
2. What makes `volatile-lru` different from `allkeys-lru` in code? One line.
3. Why does the eviction pool improve on pure sample-of-5? What property must its insertion maintain — and state the two ways your current `Push` violates it.
4. How does an 8-bit counter represent a million accesses? Why is decay required?
5. Why don't replicas evict on their own?

---

# Chapter 25 — Reading the source: eviction

> **Time:** 1.5 h · **Before you start:** Chapter 24.
>
> **You're done when:** You have read `evictionPoolPopulate` closely enough to see what your own `Push` gets wrong.


## Eviction (1.5 h)

Read: `evict.c`: `getMaxmemoryState`, `performEvictions` (the free-target loop), `evictionPoolPopulate` (insertion order! — this is your audit-#3 fix, read it twice), `LFUGetTimeInMinutes`/`LFULogIncr`/`LFUDecrAndReturn`.
What you steal: pool insertion exactly; just-enough freeing loop; LFU's three tiny functions verbatim.

# Chapter 26 — Build: maxmemory and eviction

> **Time:** 23 h · **Before you start:** Chapters 24 and 25.
>
> **You're done when:** Under memory pressure, LRU keeps a hot set resident while random visibly does not, and `DEBUG RECOUNT-MEMORY` shows zero drift.


**Goal**: byte-budget memory accounting, all eight policies, the real pool. Retires audit findings 3, 8, 12.

## 26.1 Accounting

`SizeOf()` (laid down in chapters 11 and 19) rolls into an engine counter maintained at the choke point: Set/Delete/mutations adjust by delta (mutators return size deltas — retrofit is mechanical since mutation already funnels through type-layer functions). Include: obj headers (a constant you calibrate once against `runtime.ReadMemStats` with 1M uniform keys — document the calibration in a comment), key strings, expires entries, and (later) replication backlog + client output buffers (Redis counts those too — add in ch. 30; it matters: a 256 MB replica buffer *is* memory).
`INFO memory`: `used_memory` (your counter), `used_memory_rss` (`runtime.ReadMemStats.Sys` — labeled approximate), `maxmemory`, `maxmemory_policy`, `evicted_keys`.

Retire `KeysLimit` (audit #8); `CONFIG SET maxmemory 100mb` with the size-suffix parser.

**Client eviction** (`maxmemory-clients`, 7.0) belongs here as well: client output and input buffers are counted in a separate aggregate budget, and when it is exceeded the clients with the largest buffers are disconnected — the aggregate cousin of the per-client `client-output-buffer-limit` (§38.2). Without it, ten thousand moderately-slow clients each staying under the per-client limit can still exhaust memory, which is the failure the 7.0 feature was added for. `MEMORY USAGE key [SAMPLES n]` also lands here, since it is `SizeOf()` with a sampling rule for large collections.

## 26.2 performEvictions

Port ch. 25's loop: on write-command dispatch when over budget → compute overage → loop {sample per policy (16 from your dict's honest `RandomKey`, or the expires dict for volatile-*) → pool-insert → evict pool's best → full delete hooks (propagates DEL! §24.5) → recount} until under or nothing evictable → then `noeviction`-style `-OOM` for the triggering write if still over. Reads never blocked. `CmdDenyOOM` flag from ch. 7's table decides who's a "write" here — reward collected.

## 26.3 The pool, correctly (audit #3's funeral)

Rewrite `Push` per `evictionPoolPopulate`: pool ascending by idle; binary-search rank; full+worst → skip; else insert-at-rank (memmove — it's 16 entries, a copy is fine), evicting the **least idle** end; keyset bookkeeping exact. Property tests: (a) pool always sorted, (b) keyset ≡ pool members, (c) after N random pushes, pool ≡ top-16-by-idle of all pushed. Then the LFU trio (`LFULogIncr` probabilistic bump, `LFUDecrAndReturn` decay, minutes clock) — same pool ranked by counter.

## 26.4 Done-when

```bash
# LRU quality reproduction (the antirez plot):
#   maxmemory 50mb, insert 2× that with sequential access pattern, then hot-set loop;
#   storm reports hit rate; allkeys-lru ≥ 90% hot-set hits; allkeys-random visibly worse; plot ages of evictees
redis-cli CONFIG SET maxmemory-policy noeviction ; <fill> ; SET x y → -OOM ; GET works
# volatile-lru with zero TTL keys behaves as noeviction
# under eviction pressure for 10 min: used_memory tracks maxmemory ±5%, RSS doesn't run away
# eviction propagates: (stub check now) evicted key hits the AOF as DEL/UNLINK
go test ./core/evict/... # pool property tests
```

## 26.5 Traps

- Accounting drift: every mutation path that forgets its delta shows up as slow leak vs maxmemory. Add a `DEBUG RECOUNT-MEMORY` that walks and re-sums, assert drift == 0 at the end of every storm run — cheap invariant, catches everything.
- Don't evict *while executing* the triggering command's lookup phase — evict at the dispatch gate (or you invalidate objects the command already holds — the C code evicts before `call()` for this reason; your audit-#13 Rename-eviction reentry was this bug's cousin).
- 24-bit clock wraparound: your `getIdleTime` is right; keep its test anyway — the person who "simplifies" it later is also you.

---

---
---

# PART VI — REPLICATION

The wall. Two machines, one dataset, an asynchronous link that drops.

---

# Chapter 27 — Replication

> **Time:** 5 h · **Before you start:** Chapter 26.
>
> **You're done when:** You can state the two conditions that allow a partial resync.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Replication" — `redis.io/topics/replication`; full read | 1 h |
> | `replication.c`: the file comment, `syncCommand`, `masterTryPartialResynchronization`, `syncWithMaster` (the replica-side state machine — the longest function you'll read in this project) | 2.5 h |
> | `tests/integration/replication-psync.tcl` — partial-resync spec | 45 min |
> | Kleppmann, *Designing Data-Intensive Applications*, **ch. 5 (Replication)** — read the "Problems with Replication Lag" and "leaderless" sections against what you just built. The single best framing of the trade Redis is making. | 1.5 h |

## 27.1 The model

- **Asynchronous.** Master replies to the client, *then* streams to replicas. An acknowledged write can die with the master. (`WAIT numreplicas timeout` lets a client demand N acks — synchronous replication on request, still not consensus: no rollback, no election safety.)
- **Replicas are read-only** (`replica-read-only yes`) and can serve stale reads; `INFO replication` exposes lag.
- **Chained**: a replica can have sub-replicas; it forwards the master's stream verbatim (it does *not* re-generate it — important: sub-replicas need the master's exact byte stream, offsets included).
- One master per replica; `REPLICAOF host port` switches (and `REPLICAOF NO ONE` promotes).

## 27.2 The three identifiers

Everything in replication reduces to bookkeeping of three values:

- **Replication ID** (`replid`) — 40 hex chars naming a *history*. A master mints one at boot; a promoted replica mints a new one (it's starting a new history) but remembers the old as `replid2` for a grace window (so siblings that followed the old master can still partially sync).
- **Offset** — byte count into that history's command stream. Master increments as it writes the stream; each replica tracks the offset it has applied, and reports it back (`REPLCONF ACK <offset>`, every second — feeds `WAIT` and `min-replicas-to-write`).
- **Backlog** — one circular buffer (default 1 MB, size it in config) on the master holding the most recent stream bytes, allocated when the first replica connects. Disconnection ≤ backlog-window → partial resync.

`ReplID + offset` is a **position in a replicated log** — Raft's (term, index) with the safety guarantees removed. Seeing that mapping is half of what this stage teaches; the other half is what its absence costs (§27.6).

## 27.3 The handshake, byte by byte

Replica connects and speaks (replica-side state machine: `syncWithMaster`):

```
PING                                → +PONG            (liveness)
REPLCONF listening-port <port>      → +OK              (for INFO/failover)
REPLCONF capa eof capa psync2       → +OK              (capabilities)
PSYNC <replid> <offset>             → one of:
   +CONTINUE <replid>          partial: master replays backlog from offset; done
   +FULLRESYNC <replid> <off>  full: RDB transfer follows, then live stream
```

First-ever sync sends `PSYNC ? -1` (no history yet) → always FULLRESYNC. The master side (`syncCommand` → `masterTryPartialResynchronization`) accepts CONTINUE iff: replid matches (or matches replid2 within its window) **and** the requested offset is still inside the backlog. Else full.

**Full resync mechanics:** master starts a BGSAVE *with the replica's socket attached as a waiter*; if a BGSAVE is already running for another waiting replica, it can piggyback (one snapshot serves many). RDB is streamed either from disk or **diskless** (`repl-diskless-sync`, child writes straight to sockets; `$EOF:<40-byte-delim>` framing since the length is unknown upfront). *Every write executed during the save is queued per-replica* and flushed after the RDB — so the replica ends exactly at the offset the stream continues from. Replica-side: flush its own data, load the RDB (serving `-LOADING` errors meanwhile, or old data per `replica-serve-stale-data`), then apply the queued stream.

## 27.4 The steady state

After sync, the master treats the replica as *a client that receives but never asks*: every effective write's propagated form (§20.4 — the same rewritten stream the AOF gets) is appended to each replica's output buffer and to the backlog, offset advancing per byte (`replicationFeedSlaves`). Replica applies commands through the fake-client path (no replies generated, `flags |= CLIENT_MASTER`), tracks its applied offset, ACKs each second.

Two periodic frames ride the same stream: master pings replicas (`repl-ping-replica-period 10`) so idle links advance offsets and prove liveness; replicas that miss ACKs long enough are dropped (`repl-timeout`).

**The buffer-limit interaction** (§38.4): a slow replica's output buffer grows on the master; past `client-output-buffer-limit replica 256mb 64mb 60`, the master kills the link — the replica reconnects and (hopefully) partial-syncs from the backlog. Under-size both and you get the classic **full-resync loop**: slow replica → buffer kill → reconnect → full sync (heavy) → even slower → repeat. You will reproduce this on purpose in ch. 30's chaos test, because recognizing it in the wild is worth an entire ops-year.

## 27.5 Where replication hooks the loop

- Feed: inside command propagation (same call site as AOF feed — one `propagate()` fans to both).
- ACK processing / timeout checks / reconnects: `replicationCron` (1/sec, from serverCron).
- Sending: same output-buffer machinery as normal clients, drained in `beforeSleep`.

Under your engine-goroutine model: the engine owns feed + backlog; per-replica writer goroutines own their sockets; `replicationCron` is a case in the engine's ticker. No new concurrency appears — replication is *bookkeeping*, not parallelism.

## 27.6 What asynchrony costs — know it cold

- Master accepts `SET k 1`, acks, dies before feeding replicas. Failover promotes a replica. `k=1` is gone *after acknowledgment*. Redis's position: acceptable for its domain; use `WAIT` or use a CP system when not.
- **Split brain**: old master isolated with clients still writing to it; failover promotes a replica; partition heals; old master is demoted and **wipes its divergent writes** (it full-resyncs from the new master, replacing its dataset — the writes made during the partition are simply destroyed). `min-replicas-to-write N` + `min-replicas-max-lag` shrinks this window (a master that can't reach N fresh replicas refuses writes) — it's a mitigation, not a proof.
- Compare with Raft (ConsulMe ch. 23): Raft refuses the ack until a quorum has the entry. Redis takes the ack-first path deliberately: this *is* the CAP trade made concrete in two adjacent projects on your disk.

## Self-check — Chapter 27

1. What triple identifies a stream position? Map each element to its Raft cousin and name the guarantee Raft adds.
2. Exactly which two conditions admit +CONTINUE? Which config sizes the window?
3. During full resync, where do writes-executed-during-BGSAVE go, and what property does that queue guarantee about the replica's final state?
4. Why must a promoted replica change its replid? Why keep replid2?
5. Describe the full-resync loop failure and both configs implicated.
6. Why does a chained sub-replica need the master's *verbatim* stream rather than a re-generated one?

---

# Chapter 28 — Debugging a database

> **Time:** 2 h · **Before you start:** Chapter 27.
>
> **You're done when:** You have a log-merge script and a `--seed` flag ready.


Read before chapter 30; re-read when stuck there. Same discipline as ConsulMe's debugging chapter, adapted.

## 28.1 Observe, don't reason

Single-process bugs yield to reasoning. Replication/cluster bugs are *interleavings* — you will not deduce them from source; you will see them in logs or not at all. Instrument first, hypothesize second.

## 28.2 Log every state transition, and only transitions

`REPL_STATE: CONNECTING→RECEIVE_PONG`, `PSYNC sent replid=… offset=…`, `+CONTINUE granted (backlog span …)`, `FAIL conviction: node … (7/10 masters)`, `election: epoch=42 rank=1 delay=1173ms`, `snapshot: shallow-copy 1.2M keys 87ms`. Fields: timestamp(µs), node-id, subsystem, from→to, cause. Steady-state spam (every SET) belongs in MONITOR, not logs.

## 28.3 Merge logs across nodes

`sort -m` by timestamp with node-id prefixes (one machine → one clock → no ConsulMe-style skew problem; enjoy it). `scripts/mergelogs.sh *.log | less` is the cockpit for chapter 36. The bug is always visible in the merged view at the first line where two nodes' beliefs diverge — train yourself to find that line.

## 28.4 Determinism levers

- Engine model = one interleaving source (the channel). A `--record` flag that logs every dequeued command with a sequence number, and a `--replay file` that feeds them back = deterministic reproduction of any single-node bug, *including* cron timing if you log tick boundaries. Build in ch. 30's despair; ~2 h.
- Seeded randomness everywhere (`--seed`): eviction sampling, skiplist levels, election jitter. One flag, whole-run determinism.
- The AOF is already a replay log — `dice --replay-aof file` onto a debug build is time-travel.

## 28.5 Invariant assertions

`DEBUG CHECK` (build it): dict count == keyspace stat; expires ⊆ keyspace; skiplist↔dict member parity per zset; span-sum == length; memory recount == counter; replica: applied-offset == received-offset. Run in every test teardown + after chaos. A failed invariant near the cause beats a wrong answer far from it.

## 28.6 The tools you already have (use on DiceMe)

`redis-cli -p 7379 --latency` / `--latency-hist` · MONITOR (yours) · `pprof` (goroutine leaks after churn = your #1 Go-specific bug class; heap for the accounting drift) · `go test -race` on everything, always (the engine model makes races *rare*, not impossible — the snapshot handoff and the writer goroutines are where they hide) · `nc` + a text file of RESP for reproducing exact byte sequences · `hexdump -C dump.rdb` next to real Redis's.

## 28.7 The five bugs you'll actually hit

1. **Args aliasing under pipelining** (ch. 7): garbled keys under -P 16 only. Copy on channel-cross.
2. **Missing propagation on a delete path** (ch. 23/5): replica diverges by one key, hours later. The digest-compare in the drill matrix exists for this; grep all `Delete` callers.
3. **Offset drift** (ch. 30): +CONTINUE then parse garbage. Count only canonical-stream bytes; log offsets at every feed.
4. **Snapshot isolation leak** (ch. 23): one mutator skips copy-on-write; RDB contains a value from the future. The §23.3 per-type test matrix; add types to it as they're added.
5. **Wake-order violation** (ch. 33): BLPOP client observes a half-applied MULTI. Two-phase ready-keys; the §31.3 ordering test.

## 28.8 The discipline

One hypothesis at a time, written down before testing (NOTES.md). Every fixed bug gets: the invariant that would have caught it (add to DEBUG CHECK), the log line that would have shown it (add), and an Appendix-F entry (write). Never fix a distributed bug you can't reproduce — the fix is noise until the repro exists (the §28.4 levers exist to make "can't reproduce" rare).

---
---

# Chapter 29 — Reading the source: replication

> This is effectively the chapter-30 spec.
>
> **Time:** 3 h · **Before you start:** Chapters 27 and 28.
>
> **You're done when:** You have the replication handshake written out as pseudocode in your own notes.


## Replication (3 h — the long one)

Read: `replication.c` in this order: file comment → `replicationFeedSlaves` (offset+backlog+fanout) → `feedReplicationBuffer` → `syncCommand` → `masterTryPartialResynchronization` (the CONTINUE gates) → `replicaStartCommandStream` → then the replica side: `syncWithMaster` state machine end to end → `readSyncBulkPayload` (RDB receive, diskless EOF mark) → `replicationCron`. Skim `WAIT` impl (`waitCommand`, `replicationRequestAckFromSlaves`).
What you steal: everything. This tour is effectively the chapter 30 spec; take notes as pseudocode and the build becomes transcription.

# Chapter 30 — Build: replication — the wall

> The wall.
>
> **Time:** 55 h · **Before you start:** Chapters 27, 28, 29.
>
> **You're done when:** A dropped link reconnects with `+CONTINUE` and no RDB transfer, and master and replica digests match after a benchmark.


**Goal**: master + replica roles, PSYNC full and partial, backlog, correct propagation, ACK/WAIT, surviving link chaos. Read ch. 27 and ch. 29 immediately before. This phase has the highest bug-hours-per-line in the project; ch. 28's discipline (log every state transition, merge logs) is not optional here.

## 30.1 Order of work

1. **Propagation stream** (even with zero replicas): the engine builds the canonical effect stream — §20.4 shapes, offset counter, ring-buffer backlog. Unit-test the ring hard (wrap, overwrite, read-from-offset, offset-out-of-range).
2. **REPLICAOF + full sync**: replica-side state machine (connect → handshake → receive RDB via §23.3-powered BGSAVE on master → flush → load → stream). Master-side: `syncCommand`, per-replica during-save queue.
3. **Steady state**: feed replicas from propagation; replica applies via fake-client (+`CLIENT_MASTER` flag: no reply, no re-propagation... except onward to *its* sub-replicas — verbatim forwarding, §27.1); ACK every second; `INFO replication` both sides.
4. **Partial resync**: replid/replid2 + backlog window gates → `+CONTINUE`; the offset bookkeeping *will* be wrong the first four times; the byte-level log-merge (ch. 28) is how you find it.
5. **WAIT**, `ROLE` (returns `master`/`slave` plus offsets and the peer list — the pre-INFO way clients discover topology, and three lines once the state exists), `min-replicas-to-write`, replica-read-only (`-READONLY You can't write against a read only replica.`), expiry rule enforcement (§9.4 — the replica branch of expireIfNeeded finally goes live), replica eviction off.
6. **Chaos**: the drill matrix below.

## 30.2 Design notes

- The engine owns: backlog, replica registry, offsets. Each replica connection = a client with `CLIENT_SLAVE` flag whose writer goroutine drains the feed; slow replica → output-limit kill (§27.4) — implement the limit *now*, the full-resync-loop chaos drill needs it.
- Replica side is a *client of the master*: one goroutine runs the §27.3 state machine and then becomes a reader pumping the master's stream into the engine as fake-client commands. Reconnect-with-backoff on any error, re-entering at PSYNC with saved (replid, offset).
- Offset discipline: the offset counts **bytes of the canonical stream**, master-side; the replica counts bytes *as received and applied*. PINGs from master ride the stream and advance offsets (that's their point). Never count bytes of anything else.
- RDB transfer: length-prefixed from your snapshot file. Diskless (`$EOF:<delim>`) — stretch.
- Persist nothing about replication state except (per Redis) an RDB-carried replid/offset — actually simplest correct: on clean shutdown as replica, forget; always PSYNC afresh with what memory holds. (Real Redis persists more for restart-partial-resync; stretch.)

## 30.3 Done-when

```bash
# 2 terminals: dice-master :7379, dice-replica :7380 (REPLICAOF localhost 7379)
redis-cli -p 7379 SET k v ; redis-cli -p 7380 GET k          # v, within 10ms (measure the lag)
redis-cli -p 7380 SET x 1                                     # -READONLY …
redis-benchmark -p 7379 -n 100000 -P 16 -t set ; # replica DBSIZE converges to master's
# partial resync: tcpkill/close the replica's conn mid-benchmark (a storm flag drops it);
#   master log: "+CONTINUE"; NO RDB transfer (log proves it); datasets converge (`DEBUG DIGEST` — build the real command, ch. 48 §48.1: an order-independent digest of the whole keyspace, compared master-to-replica, never to a constant)
# backlog overflow: stop replica 60s under heavy writes with tiny repl-backlog-size → reconnect does FULL resync; converges
# expiry: SET k v PX 900 on master; replica GET k at t+950ms → nil BUT replica DBSIZE still counts it until master's DEL arrives (test the asymmetry!)
# WAIT: WAIT 1 100 returns 1 with live replica; kill replica → WAIT 1 100 returns 0 after ~100ms
# chained: replica-of-replica :7381 converges; digests all equal
# the full-resync loop, reproduced: tiny replica output limit + slow replica (storm --slow-reader) → observe kill/reconnect/fullsync cycle in logs; then fix by raising limit; document in NOTES.md
# kill -9 master; restart (AOF); replicas full-resync (new replid) and converge
```

## 30.4 Traps

- The #1 bug: feeding replicas the client's *verbatim* command instead of the canonical effect (SPOP, INCRBYFLOAT, EX-relative). Your ch. 23 propagation hook already canonicalizes — replication must tap the *same* stream, one producer. If you build a second path "just for replication," you will diverge; don't.
- The #2 bug: off-by-N offsets from counting reply bytes, or from the handshake's RDB bytes (the RDB is NOT part of the stream; offset starts after). Symptom: +CONTINUE followed by protocol garbage on the replica — it's parsing mid-command.
- Replica applying a command that fails (WRONGTYPE from divergence): real Redis logs and *panics* on inconsistency in strict mode; at minimum log loudly and count — silence buries the real bug.
- BLPOP-served-by-replicated-LPOP arrives (§13.3): the replica has no blocked client; the stream's RPUSH+LPOP must leave its list state identical. If your list is "empty ⇒ key deleted" (ch. 19 trap) on both sides, it works; test proves it.
- Your everysec-fsync bio goroutine + replica feed both hang off the engine tail — starving one under load is easy; the lag measurement in done-when is the canary.

---

---
---

# PART VII — TRANSACTIONS AND CLUSTER

Atomic batches and blocked clients, then sharding across many masters with automatic failover.

---

# Chapter 31 — Transactions, blocking, and pub/sub

> **Time:** 3 h · **Before you start:** Chapter 30.
>
> **You're done when:** You can say exactly when a blocked BLPOP client wakes, relative to the pushing client's MULTI batch.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `multi.c` — all of it, it's short; `blocked.c`: file comment + `blockForKeys` / `handleClientsBlockedOnKeys` | 1.5 h |
> | Redis docs: "Transactions" (`redis.io/topics/transactions` — the WATCH/CAS section) + "Pub/sub" (`redis.io/topics/pubsub`) | 45 min |

## 31.1 MULTI/EXEC/WATCH

```
WATCH inventory            +OK        remember: this client watches this key
MULTI                      +OK        client enters queueing mode
DECR inventory             +QUEUED    validated (exists? arity?), queued, NOT executed
RPUSH orders o42           +QUEUED
EXEC                                  run all atomically — or, if a watched key
                                      changed since WATCH: *-1 (null), nothing ran
```

Mechanics, all in `multi.c` + a hook:

- MULTI sets `CLIENT_MULTI`; later commands are **queued** after static validation (unknown command or bad arity marks `CLIENT_DIRTY_EXEC` → EXEC aborts with `-EXECABORT`). Runtime errors (WRONGTYPE on a queued command) do **not** abort: EXEC runs everything, errors are returned in-position, the rest still execute. **No rollback exists.** Say it again: a transaction with a runtime error in the middle commits everything else.
- WATCH registers (client, key) in `db->watched_keys` (a dict key→list-of-clients). **Every keyspace write** calls `touchWatchedKey` — the §8.3 choke point — flagging every watcher `CLIENT_DIRTY_CAS`. EXEC checks the flag: dirty → null reply, clean → execute the queue as one uninterruptible batch. This is optimistic concurrency control — check-and-set at the keyspace level. (Subtlety your tests must cover: a key that *expires* between WATCH and EXEC also touches watchers — expiry is a write, §9.2.)
- EXEC of N commands propagates wrapped in MULTI/EXEC so replicas/AOF apply it atomically too.
- Under the engine-goroutine model, "uninterruptible" is free: EXEC is one engine message that runs N evals before the next message is received.

## 31.2 Why no interactive transactions

Queued commands can't read (you get `+QUEUED`, not results), so no read-modify-write *inside* a transaction. That's what WATCH (optimistic loop: WATCH→GET→MULTI→SET→EXEC→retry-on-null) and Lua (§31.4, read-modify-write server-side) are for. An interactive transaction would hold the single thread hostage mid-transaction across client round trips — architecturally impossible by design. This question ("how do I do read-then-write atomically in Redis?") is a canonical interview probe; you now have the three-part answer.

## 31.3 Blocking commands

`BLPOP k 5`: if `k` has an element — normal LPOP. Else the client **blocks** (the *client*, never the server): flagged blocked, registered in `db->blocking_keys` (key→list of waiting clients), removed from normal read processing; the loop spins on serving everyone else.

Wake-up is the elegant part — **no polling**:

- Any write that makes a key ready (`RPUSH k`, or a RENAME creating `k`…) calls `signalKeyAsReady` — the choke point again — appending to a server-level `ready_keys` list.
- **After the current command (and its full MULTI batch, and its propagation) completes**, `handleClientsBlockedOnKeys` walks `ready_keys`, matching FIFO-blocked clients to available data.
- Ordering rules your tests must pin: waiters served FIFO (first blocked, first popped); the push and the pop are *separate* propagated events (replica sees `RPUSH` then `LPOP` — a blocked BLPOP is propagated as the LPOP it eventually performs); a `MULTI{RPUSH; LPOP}` batch never wakes a blocked client with data it already consumed (wake-up strictly after the whole batch — this ordering is why the hook appends to a list processed later rather than waking inline).
- Timeout: `beforeSleep`/cron scans blocked clients past deadline → null reply.

Family: BLPOP/BRPOP/BLMOVE, BLMPOP, ZSET variants (BZPOPMIN…), XREAD BLOCK, and WAIT (blocks on replica ACK-offset instead of a key). Build BLPOP + WAIT; the rest are the same plumbing.

In Go: a blocked client's connection goroutine simply parks on its reply channel; "blocked" is a registry entry in the engine. No goroutine gymnastics — the registry *is* the mechanism, same as C.

## 31.4 Pub/sub

Fire-and-forget fan-out, entirely orthogonal to the keyspace: `SUBSCRIBE ch` (registry channel→clients), `PUBLISH ch msg` (`pubsubPublishMessage`: push to every subscriber's output buffer *now*), `PSUBSCRIBE news.*` (pattern list, matched per publish — O(patterns), unavoidable). No buffering, no replay, no acks: a subscriber that was disconnected missed the message, full stop (contrast Streams, ch. 41–42, which are the durable variant). Subscribed clients in RESP2 enter a restricted mode (only [P]SUB/UNSUB/PING allowed); RESP3 lifts this via push frames. Messages to replicas: PUBLISH propagates through the replication stream, so subscribers on replicas hear it. The real cost center is slow subscribers × fan-out = output-buffer blowup → the `pubsub` class of client-output-buffer-limits exists precisely for this (§38.4).

Cluster wrinkle: plain `PUBLISH` must broadcast to **every node** in the cluster (a subscriber could be anywhere) — an O(nodes) bus storm per message. **Sharded pub/sub** (7.0: `SSUBSCRIBE`/`SPUBLISH`/`SUNSUBSCRIBE`, `pubsub.c:spublishCommand`) hashes the channel name like a key: messages route only to the channel's slot-owner shard and its replicas. Same registries, one extra slot check — built in ch. 36 once the bus exists, and required, because 7.0+ client libraries in cluster mode use `SSUBSCRIBE` by default.

## 31.5 Keyspace notifications

`notify-keyspace-events` turns writes into pub/sub events on `__keyspace@0__:<key>` (what happened to this key) and `__keyevent@0__:<event>` (which key had this event) — e.g. every `expired`, `evicted`, `set`, `lpush`. Off by default (costs a publish per write). Implementation: one `notifyKeyspaceEvent(type, event, key)` call at — say it with me — the keyspace choke point. Build in ch. 33: it's ~30 lines *because* the choke point exists, and it makes chaos tests observable (`SUBSCRIBE __keyevent@0__:expired` is how you watch your active-expiry cycle work in real time).

## 31.6 Lua scripting, previewed (built in ch. 39–40)

`EVAL script numkeys k… arg…` runs Lua atomically server-side, calling back via `redis.call`. It is the third answer to the read-modify-write question this chapter opened with — after WATCH and after "you can't" — and chapters 39 and 40 build it in full. Preview the two facts that matter here: a script is atomic for the same reason `EXEC` is (one execution context, held), and it **propagates by effects** (§20.4), so what reaches the replica is the writes the script performed, never the script text. Everything you built in this chapter — the queue-then-run shape, the propagation batch, the deferred wakeups — is reused verbatim.

## Self-check — Chapter 31

1. Queue-time error vs runtime error inside MULTI — different EXEC outcomes. State both and justify the second.
2. Implement WATCH in one sentence given the §8.3 choke point. Why does an *expiry* touch watchers?
3. Why exactly can't Redis offer read-your-queued-writes transactions? What are the two sanctioned substitutes?
4. When precisely does a blocked BLPOP client wake, relative to the RPUSH's MULTI batch and its propagation? Why not inline?
5. What does a replica see when a blocked BLPOP is eventually served?
6. Pub/sub vs Streams in one line each. Which limit protects the server from slow subscribers?

---

# Chapter 32 — Reading the source: MULTI, blocking, and pub/sub

> **Time:** 1.5 h · **Before you start:** Chapter 31.
>
> **You're done when:** You can explain the two-phase ready-keys wakeup.


## MULTI, blocking, pubsub (1.5 h)

Read: `multi.c` whole; `blocked.c`: `blockForKeys`, `handleClientsBlockedOnKeys` (the after-command placement + FIFO), `unblockClient`; `pubsub.c`: subscribe/publish paths; `notify.c` (tiny).
What you steal: dirty-CAS flag mechanics; ready-keys two-phase wakeup; the propagate-the-effective-pop rule; notify's single entry point.

# Chapter 33 — Build: transactions, blocking, pub/sub

> **Time:** 27 h · **Before you start:** Chapters 31 and 32.
>
> **You're done when:** 1000 scripted WATCH races produce exactly one winner each, and BLPOP wakes on the push.


**Goal**: MULTI/EXEC/DISCARD/WATCH/UNWATCH, BLPOP/BRPOP/BLMOVE/WAIT-style blocking, SUBSCRIBE family, keyspace notifications. Ch. 13 is the spec; the choke-point hooks stubbed since ch. 11 all go live. This phase is where the engine-goroutine choice (§2.4) pays out — estimate says 25 h *because* of it.

## 33.1 Build notes

- **MULTI**: flags + queue on the Client (fields exist since ch. 7); dispatch gate: in-MULTI × not-{EXEC,DISCARD,MULTI,WATCH,RESET} → validate-and-queue (+QUEUED / flag DIRTY_EXEC on validation error). EXEC: gates (DIRTY_EXEC → EXECABORT; DIRTY_CAS → null), then run the queue inline as one engine message; wrap propagation in MULTI/EXEC (via the effect mechanism — a batch effect).
- **WATCH**: `watched_keys` dict on the db + list on client; `touchWatchedKey` in the ch. 11 hook slot; fires on every effective write **including expiry and eviction deletes** (they already funnel — reward). UNWATCH + auto-unwatch after EXEC. Cross-db WATCH if you did SELECT — or reject WATCH on db>0, documented.
- **Blocking**: registry per §31.3 (`blocking_keys`, `ready_keys`); the two-phase wake (signal at choke point → serve after current message completes — in engine terms: collect ready keys during dispatch, process them at message tail, before AOF flush hooks); FIFO per key; timeout wheel = simplest: check blocked clients in cron tick (100 ms granularity is fine; Redis is similar). Propagate the effective pop (§13.3). BLPOP/BRPOP/BLMOVE + WAIT (blocks on ack offsets — replication integration test in one command); then BLMPOP/BZPOPMIN/BZPOPMAX/BZMPOP — same registry, different pop function (the zset ones signal-ready from ZADD, proving the choke point generalizes).
- **Pub/sub**: two registries (channels, patterns — pattern match with your ch. 11 glob), publish = immediate buffer pushes + count reply; PUBSUB CHANNELS/NUMSUB/NUMPAT introspection; propagate PUBLISH to replicas (subscribers on replicas hear it — test). **Protocol split**: in RESP2 a subscribed client enters restricted mode (only [P]SUBSCRIBE/[P]UNSUBSCRIBE/PING/QUIT/RESET), and messages arrive as ordinary arrays; in RESP3 there is no restriction and messages arrive as **push frames** — the frame type your ch. 7 serializer already emits, finally used. Both behaviours, selected by the connection's `HELLO` version, and both diffed.
- **`RESET`**: one command that unwinds everything a connection accumulated — discards MULTI state, UNWATCHes, exits subscriber mode, unsets the client name, resets the protocol to RESP2, deauthenticates, and returns `+RESET`. It is trivial and it is what client-library connection pools call before returning a connection to the pool, so a missing `RESET` shows up as bizarre cross-request state bugs in somebody else's code.
- **Notifications**: `notify-keyspace-events` config parsing (the flag-letters), `notifyKeyspaceEvent` in the hook slot, events per command class as documented. ~30 lines *because* the architecture (told you in §31.5).

## 33.2 Done-when

```bash
tests/harness/diff.sh tests/ch33_multi.txt      # queue/abort/dirty matrices vs real redis
# WATCH races: two raw-conn Go clients, scripted interleave → exactly one EXEC null; loop ×1000 deterministic
# WATCH + expiry: WATCH k (TTL 100ms) → sleep → EXEC → null
# BLPOP: A blocks; B pushes; A wakes with value; replica saw RPUSH,LPOP; FIFO with 3 blockers; timeout returns null-array at ±100ms
# BLPOP inside MULTI does NOT block (returns immediately/null — real semantics: it degrades; diff it!)
# pub/sub: 100 subscribers × 10k messages, ordering per subscriber preserved; pattern overlap gets both deliveries
# notifications: __keyevent@0__:expired observed for an active-cycle expiry (watch your own machinery on the wire!)
```

## 33.3 Traps

- EXEC's queue runs even with runtime errors mid-batch (§31.1); only *queue-time* validation aborts. The diff file must include both matrices.
- Wake-before-propagation-complete ordering bug: a blocked client served *within* B's MULTI batch observes a half-applied batch. Your two-phase design prevents it; the test is the MULTI{RPUSH,LPOP} case from §31.3.
- Client disconnect while blocked / while in MULTI / while subscribed — the ch. 7 cleanup function grows its last three tentacles; leak-test each.
- WAIT inside MULTI: returns immediately with current acks (can't block) — obscure, diff it anyway.

---

# Chapter 34 — Cluster

> **Time:** 4 h · **Before you start:** Chapter 33.
>
> **You're done when:** You can explain MOVED versus ASK and what each does to the client's slot map.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Scale with Redis Cluster" (`redis.io/topics/cluster-tutorial`) + "Redis cluster specification" (`redis.io/topics/cluster-spec`) — the spec is the assignment | 2 h |
> | `cluster_legacy.c`: `clusterProcessPacket` (the gossip switchboard), `clusterHandleSlaveFailover`; `cluster.c:getNodeByQuery` (the redirect decision) | 1.5 h |
> | **Das, Gupta, Motivala, "SWIM: Scalable Weakly-consistent Infection-style Process Group Membership Protocol"** (DSN 2002) — `cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf`. Cluster's PFAIL/FAIL gossip is SWIM's shape with different names; 6 pages, §3–4 are the protocol. If you did ConsulMe you have already built this. | 1 h |

Cluster answers a different question than Sentinel: not "who replaces the master" but **"how do 10 masters share one keyspace"** — sharding first, HA integrated.

## 34.1 Slots

The keyspace is split into **16384 slots**: `slot = CRC16(key) mod 16384`. Every master owns a set of slots; ownership is the unit of resharding. Why 16384 and not 2^32: the cluster gossip carries each node's slot set as a **2 KB bitmap** (16384/8); more slots = fatter heartbeats for no benefit at realistic node counts (≤1000).

**Hash tags**: if the key contains `{…}`, only the braced part hashes — `user:{42}:profile` and `user:{42}:cart` share a slot. This is the *only* multi-key mechanism in cluster: multi-key commands (MSET, SINTERSTORE, MULTI batches, Lua) require all keys in **one slot**, else `-CROSSSLOT`. Design consequence worth internalizing: cluster mode moves the join problem to the client's key schema.

## 34.2 Redirects: MOVED and ASK

Any node answers any query — with a redirect if it doesn't own the slot:

- `-MOVED 3999 10.0.0.7:6379` — "slot 3999 permanently lives there." Client updates its slot→node cache and retries. Smart clients (`redis-cli -c`, every production library) bootstrap the full slot map via `CLUSTER SHARDS`/`SLOTS` and go straight to the right node thereafter.
- `-ASK 3999 10.0.0.8:6379` — "for *this key, this once*, ask there" — the migration protocol (§34.4). Client retries at the target prefixed with `ASKING`, and does **not** update its map.

The server side is one function: `getNodeByQuery` — compute slot(s), verify single-slot, check ownership/migration state, return node-or-error. Your ch. 36 port of it is ~80 lines and the heart of the phase.

## 34.3 The cluster bus

Node-to-node runs on a second port (client port + 10000), speaking a **binary protocol** (not RESP): fixed header (`clusterMsg`: sender id/epochs/slot bitmap/flags) + typed body. Message types: PING/PONG (gossip carriers), MEET (join), FAIL (conviction broadcast), PUBLISH (cluster-wide pub/sub), FAILOVER_AUTH_REQUEST/ACK (election votes), UPDATE (stale-config correction).

Gossip: each node pings a few random peers per second (plus any peer silent > timeout/2); every PING/PONG **piggybacks a sample of the sender's view of other nodes** (address, flags, last-pong). Failure detection is ConsulMe ch. 5 vocabulary re-spelled: no pong > `cluster-node-timeout` → mark **PFAIL** (≈suspect); gossip accumulates others' PFAIL reports; majority-of-masters agreement → **FAIL** (≈dead), broadcast immediately. You built exactly this once — the ch. 36 gossip is your SWIM toy with a slot bitmap bolted on.

## 34.4 Resharding

Moving slot S from node A to B, live:

```
CLUSTER SETSLOT S IMPORTING A   (on B)
CLUSTER SETSLOT S MIGRATING B   (on A)
loop: CLUSTER GETKEYSINSLOT S count  (on A)
      MIGRATE B-host B-port "" 0 timeout KEYS k1 k2…   (atomic per batch:
          serialize (DUMP format = RDB value + version + CRC) → RESTORE on B → DEL on A)
CLUSTER SETSLOT S NODE B        (on both/all — bumps epoch)
```

During migration: A still owns S; a key present on A is served by A; a key *absent* on A (maybe already moved) → `-ASK` to B. B accepts keys for S only after `ASKING`. This is why ASK ≠ MOVED: ownership hasn't changed yet, only residence of some keys. When `SETSLOT NODE` lands, stragglers learn via MOVED and epoch-stamped UPDATE messages.

`MIGRATE` is synchronous per batch — the one place cluster chooses consistency over latency (a key is never in two places from a client's viewpoint).

## 34.5 Failover inside cluster

Sentinel-less: replicas of a failed master run the election themselves.

1. Replica sees its master FAIL. Eligibility: its link freshness (`cluster-replica-validity-factor` × node-timeout) — a too-stale replica won't self-promote.
2. Waits a **rank-staggered delay** (500 ms + random + rank×1s, rank = position by replication offset — freshest first) — better replicas ask first; a poor-man's leader-preference, not a safety rule.
3. Broadcasts FAILOVER_AUTH_REQUEST with a bumped `currentEpoch`. **Masters** (only masters vote; only one vote per epoch; only if the requester's slots' `configEpoch`s aren't stale) reply ACK.
4. **Majority of masters** → replica promotes: new `configEpoch` (now the highest), claims the slots, broadcasts PONG. Old master on return sees higher epoch on its slots → demotes to replica.

Same skeleton as Raft's election (epoch, one-vote, majority) but guarding **slot ownership**, not a log — data loss windows of §27.6 persist (async replication underneath is unchanged). `cluster-require-full-coverage`: by default, *any* slot uncovered → whole cluster refuses queries (surprising, frequently disabled; know both behaviors).

Epoch collisions (two masters claiming the same configEpoch, possible after resharding races) are resolved by a deterministic rule: the node with the smaller ID bumps — read `clusterHandleConfigEpochCollision`.

## 34.6 What you build in ch. 36

Full slots/MOVED/ASK/resharding + gossip + FAIL conviction + the replica election with real config epochs, plus cluster-wide `PUBLISH` and sharded pub/sub (`SSUBSCRIBE`/`SPUBLISH`), because 7.0+ client libraries in cluster mode send the sharded forms by default.

**One deliberate incompatibility**: the bus's *wire format*. You implement the protocol's logic over your own node-to-node encoding rather than reproducing `clusterMsg` byte for byte. Clients never speak the bus, so an all-DiceMe cluster is a complete replacement from every client's point of view; a *mixed* DiceMe/Redis cluster is not. Write that sentence in your README (§48.4 makes you) rather than letting someone discover it. Byte-compatibility is a well-scoped ~20 h follow-on if you ever need it.

A 6-node local cluster surviving `kill -9` of a master, with `redis-cli -c` following redirects — that's the exit demo.

## Self-check — Chapter 34

1. Why 16384 slots? What message field does the number size?
2. MOVED vs ASK — state what each changes in the client, and why migration *needs* the weaker one.
3. Which nodes vote in a failover election, what are the three gates on granting a vote, and what does majority-of-masters protect against that quorum-of-anyone wouldn't?
4. Where exactly is the atomic step in resharding a slot?
5. A partitioned minority holds master M for slot 7. Clients write to M. The majority elects M's replica. Heal. Trace M's dataset, step by step.
6. Your SWIM toy from ConsulMe: name two things cluster gossip adds and one SWIM refinement (from Lifeguard) it lacks.

---

# Chapter 35 — Reading the source: cluster and Sentinel

> **Time:** 2.5 h · **Before you start:** Chapter 34.
>
> **You're done when:** You can name the three gates on granting a failover vote.


## Cluster & Sentinel (2.5 h)

Read: `cluster.c`: `getNodeByQuery` (the whole redirect brain); `cluster_legacy.c`: `clusterProcessPacket` (top switch), `clusterSendPing` (gossip-section sampling), `markNodeAsFailingIfNeeded` (PFAIL→FAIL), `clusterHandleSlaveFailover` (election, rank delay, epoch bump), `clusterHandleConfigEpochCollision`; `crc16.c` (your port is 30 lines). Sentinel (the spec for your ch. 45 build): `sentinelCheckObjectivelyDown`, `sentinelStartFailover` + the state machine around it.
What you steal: `getNodeByQuery` logic; PFAIL/FAIL conviction rules; election gates; the epoch-collision tiebreak.

---
---

# Chapter 36 — Build: cluster — the second wall

> The second wall.
>
> **Time:** 59 h · **Before you start:** Chapters 34, 35, and a working chapter 30.
>
> **You're done when:** Six nodes, `kill -9` a master, automatic failover, and `redis-cli -c` follows redirects throughout.


**Goal**: slots, redirects, live resharding, gossip membership, PFAIL→FAIL conviction, replica failover with epochs. Ch. 12 + ch. 35 are the spec. Wire format is *yours* (JSON or your own binary — logic over bytes); everything else is the real protocol.

## 36.1 Order of work

1. **Slots, static**: `cluster-enabled yes`; CRC16 (port the table) + hash-tag extraction; `getNodeByQuery` port: single-slot enforcement (`-CROSSSLOT`), MOVED for foreign slots; `CLUSTER KEYSLOT/SHARDS/SLOTS/MYID/INFO`; static config bootstrap (`CLUSTER SETSLOT`-style assignment via a script) → 3 masters, `redis-cli -c` works. *Cluster mode restricts to db 0.*
2. **The bus**: second listener (port+10000); node registry (id, addr, flags, slots bitmap, epochs, last-ping/pong); MEET → handshake → gossip: periodic PING to random/oldest peers, each carrying N random gossip entries (your ConsulMe SWIM muscle, directly). New nodes converge to full membership + slot map with zero central config.
3. **Conviction**: PFAIL by timeout → gossip carries others' PFAILs → majority of masters → FAIL broadcast. (Your SWIM toy's suspicion machinery, renamed.)
4. **Resharding**: IMPORTING/MIGRATING states; ASK/ASKING; `CLUSTER GETKEYSINSLOT`; MIGRATE as DUMP→RESTORE→DEL (your RDB value-serializer from ch. 23 *is* DUMP's payload format — reward); `SETSLOT NODE` with epoch bump; a `dice-cli-reshard` script driving it live under storm.
5. **Failover**: cluster replicas = ch. 30 replication + bus membership; election per §34.5 (rank delay, masters-only votes, epoch gates, majority); promotion claims slots with new configEpoch; UPDATE messages fix stale nodes; epoch-collision tiebreak.
6. **Cluster pub/sub**: plain `PUBLISH` broadcast over the bus to every node, then sharded pub/sub — `SSUBSCRIBE`/`SUNSUBSCRIBE`/`SPUBLISH` routed by `slot(channel)` to the shard's master and its replicas, plus `PUBSUB SHARDCHANNELS|SHARDNUMSUB`. Same registries as ch. 33, one slot check (§31.4).
7. **Chaos drills.**

## 36.2 Done-when

```bash
tests/ch36_cluster.sh up          # 6 nodes, 3+3, slots 0-5460/5461-10922/10923-16383
redis-cli -c -p 7379 SET user:{42}:a 1 && GET via any node   # transparent redirects
MSET k{t}1 v k{t}2 v                # ok;  MSET a b c d → -CROSSSLOT
tests/ch36_reshard.sh 100         # move 100 slots live under storm: zero client errors (ASK path exercised — count ASKs in logs > 0)
kill -9 <master2>                   # ≤ node-timeout+2s later: its replica promoted, cluster_state:ok, writes flow; old master returns → demotes to replica (log shows epoch reasoning)
# partition drill (iptables/pf or a --drop-peer test hook): minority master's clients get errors after timeout (cluster-require-full-coverage both settings tested); heal → converge; count lost writes; write the number in NOTES.md next to §27.6
# gossip convergence: node 7 MEETs node 1 only; knows all slots+nodes < 5s
# pub/sub: PUBLISH on any node reaches a subscriber on any other; SPUBLISH reaches only the slot's shard
```

## 36.3 Traps

- Slot bitmap in gossip must be the *sender's claimed* slots; receivers believe it only with epoch ≥ what they know for those slots — skipping the epoch check makes resharding + failover corrupt each other (the UPDATE message exists for the losing side).
- ASK without ASKING must be refused by the importer (else clients that skip ASKING see keys "arrive early" — breaks the one-owner invariant).
- MIGRATE's DEL on source must be atomic with the transfer *from the client's perspective* — engine model gives it (one message does serialize+send+await+delete... careful: awaiting a network reply inside the engine blocks everything — the one place you must **not** hold the engine: mark keys "migrating-busy," do the transfer async, fail racing writes with `-TRYAGAIN` — which is exactly what real Redis returns; ch. 35 shows it).
- Election without the rank delay works fine in tests and elects the stalest replica in production-shaped chaos; keep the delay, verify by making offsets deliberately unequal in the drill.
- 6 processes' logs are unreadable separately; the ch. 28 merge script is mandatory equipment here.

---

---
---

# PART VIII — PRODUCTION

High availability, the operational surface, and the à-la-carte menu of everything left.

---

# Chapter 37 — Sentinel

> **Time:** 2 h · **Before you start:** Chapter 36.
>
> **You're done when:** You can explain SDOWN versus ODOWN and why acting needs a majority.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "High availability with Redis Sentinel" — `redis.io/topics/sentinel`; full read; the "consistency under partitions" example especially | 1 h |
> | `sentinel.c`: `sentinelCheckObjectivelyDown`, `sentinelStartFailover`, and the failover state machine around `sentinelFailoverStateMachine` | 45 min |

Sentinel is the HA layer for non-cluster Redis: N sentinel processes (run ≥3, odd) monitor masters, agree on failure, and orchestrate promotion. It is a *separate process* reusing the redis binary (`redis-sentinel`); you build `dice-sentinel` in ch. 45, and this chapter is that chapter's specification.

## 37.1 Detection: SDOWN vs ODOWN

- Each sentinel pings everything (masters, replicas, other sentinels). No pong past `down-after-milliseconds` → that sentinel marks **SDOWN** (subjectively down) — one node's opinion, ConsulMe's "suspect."
- For a *master*, a sentinel asks peers (`SENTINEL is-master-down-by-addr`); **quorum** agreeing → **ODOWN** (objectively down). Only ODOWN can trigger failover. SWIM reached the same shape (indirect confirmation before conviction) for UDP probes; convergent evolution.

Sentinels discover each other and the replica topology with zero config beyond the master: they publish to `__sentinel__:hello` on the monitored master's pub/sub every 2 s (self + master coords + config epoch) and INFO the master for replicas.

## 37.2 The election and the failover

Before acting, the sentinel that saw ODOWN asks peers to vote it **leader for this failover, in this epoch** — majority of *all* sentinels required (not just quorum), one vote per sentinel per epoch, first-come-first-served. It is Raft's election vocabulary (epochs, majority, single vote) applied to a *task lease*, not a log — after the failover, no leader persists.

The leader then (state machine in `sentinel.c`):

1. **Selects** the promotion target: exclude SDOWN/disconnected replicas, prefer `replica-priority`, then highest replication offset (least data loss), then lexicographic runid (determinism).
2. Sends it `REPLICAOF NO ONE` → waits for its INFO to say master.
3. Reconfigures other replicas: `REPLICAOF <new master>`.
4. Bumps the **config epoch**; broadcasts the new map via hello messages. Higher epoch wins all future conflicts — the old master, on return, receives `REPLICAOF` from any sentinel with the newer epoch and demotes (losing its divergent writes — §27.6).

Clients discover the current master by asking any sentinel (`SENTINEL get-master-addr-by-name`) and re-ask on disconnect — client libraries implement this; your ch. 45 build includes a 50-line Go client that does.

## 37.3 What Sentinel is not

Not consensus over data — only over *who is master*; the data-loss windows of ch. 27 all remain. Quorum < majority is legal for detection but the leader vote always needs majority (so a minority partition can *see* ODOWN but never act). Two sentinels on two nodes is worse than one: no majority when either dies. These are exactly the FLP/CAP corners ConsulMe taught — here they're configuration pitfalls instead of theorems.

## Self-check — Chapter 37

1. SDOWN vs ODOWN — and why does replica-SDOWN never trigger anything by itself?
2. Detection needs quorum; acting needs majority. Why the stronger requirement to act?
3. Rank the replica-selection criteria and justify the offset one.
4. What single number decides who wins when two failovers race? Where did you meet this idea in ConsulMe?
5. Why is 2 sentinels worse than 1?

---

# Chapter 38 — Production-grade concerns

> Roughly 2 h reading, 15 h of work.
>
> **Time:** 17 h · **Before you start:** Chapter 37.
>
> **You're done when:** Metrics, every limit in §38.2, and the chaos script are all in place.


Do the work of this chapter alongside Part IX, but *read* it now — several items change earlier designs cheaply, and the limits in §38.2 are what keep the Part IX chaos drills honest.

## 38.1 Observability

- **INFO complete**: all sections real Redis emits (server, clients, memory, persistence, stats, replication, cpu, keyspace) with the fields your subsystems already track. `INFO everything` is your ops UI; treat gaps as bugs.
- **MONITOR**: flag on client; choke point already sees every command — stream them (`+<ts> [db addr] "SET" "k" …`). 20 lines; unreasonably useful for debugging *your own* server. (Warn: real MONITOR halves throughput; yours will too; that's authentic.)
- **Metrics**: instantaneous ops/sec (rolling window), hit/miss ratio, expired/evicted counters, connected clients/replicas, repl offset lag, AOF buffer size, snapshot pause ms. Wire the `monitoring/` directory you already have: a `/metrics` HTTP listener translating `INFO` fields, and the existing Grafana dashboards pointed at DiceMe. The storm-plus-dashboard demo is the best show-someone artifact in the repo, and it costs about six hours.
- **Logs**: structured (`slog`), one line per state *transition* (ch. 28), loglevel config.

## 38.2 Limits — every one is a production incident you're pre-empting

- `maxclients` (fd exhaustion) · `proto-max-bulk-len` (memory bomb per §4.3) · `client-output-buffer-limit` per class {normal, replica, pubsub}: hard bytes / soft bytes+seconds → kill (§27.4's full-resync loop, §31.4's slow subscriber — both die here) · input-buffer cap (a client that sends MULTI and queues 10M commands) · `maxmemory-clients` (7.0 **client eviction**: total client-buffer memory budget; over it, evict the biggest-buffer clients — the aggregate cousin of per-client limits) · `timeout` (idle client reaping, cron) · `tcp-keepalive`.
- Test each limit with a hostile storm mode (`storm --slow-reader --huge-bulk --queue-bomb`). "Server survives hostile clients with bounded memory" is the phase's demo.

## 38.3 Config, done properly

Full registry: every config has {name, type, default, validator, dynamic?, apply-hook}. `CONFIG GET pattern`, `CONFIG SET` (dynamic ones), `CONFIG REWRITE` (write current state back to file — preserve comments? Redis appends a generated section; do that). Startup: file → flags → env, documented precedence. `CONFIG RESETSTAT`.

## 38.4 Protections & security basics

The full security surface — AUTH, ACLs with selectors, TLS, `protected-mode`, command disabling, the `CLIENT KILL`/`PAUSE` family — is chapters 46 and 47. What belongs *here*, now, is the operational half: never log values (audit your logs — key names fine, values never), keep the AUTH gate slot in the dispatch sequence you built in ch. 7 wired to a no-op so ch. 47 is an insertion rather than a restructure, and make sure `protected-mode`'s default is on before you ever bind to a non-loopback address on a shared machine.

## 38.5 Benchmark honestly

`make bench` table in README: dice vs real redis, same box, same `redis-benchmark` flags: PING/SET/GET/INCR/LPUSH/ZADD ×{-P 1, -P 16}. Expect: within 2-4× of C Redis single-instance; *understand* every gap you can't close (Go GC, channel hop per command ≈ 1-2 µs — measure a no-op command to isolate it, cgo-free syscalls…). A README that says "60% of C Redis and here is exactly where the other 40% goes" is worth more than the missing 40%.

## 38.6 Chaos, continuous

`storm` grows modes: mixed-type workload · TTL-heavy · pipeline bursts · slow-reader · disconnect-storm · plus a `chaos.sh` that loops: random {kill -9 a node, drop a link, fill memory, BGSAVE spam} against a running topology while a verifier client asserts invariants (acked writes durable per policy, replica convergence, cluster availability). Leave it running overnight before calling any phase done. Every failure it finds goes in Appendix F with a test.

---

# PART IX — THE REST OF REAL REDIS

Everything a client can still ask for that you have not built: server-side scripting, streams, the bit/cardinality/geo command families, Sentinel, the security surface — and the sweep that proves a real client library cannot tell DiceMe from Redis.

Nothing in this part is optional. Chapters 1–38 give you a correct Redis; this part gives you a *complete* one, and completeness is the difference between a study project and a replacement.

---

# Chapter 39 — Scripting: Lua and Functions

> **Time:** 3 h · **Before you start:** Chapter 38.
>
> **You're done when:** You can say what a script that calls `SPOP` twice sends to the replica, and why.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Scripting with Lua" and "Redis Functions" (`redis.io/docs/latest/develop/programmability/`) — the whole programmability section | 1 h |
> | `script.c`: `scriptPrepareForRun`, `scriptCall`, the flags block; `script_lua.c`: `luaRedisCallCommand`, `luaReplyToRedisReply`, `luaRegisterRedisAPI` | 1 h |
> | `functions.c`: `functionsRegisterEngine`, `fcallCommand`, `functionsSaveRio`; `function_lua.c` skim | 30 min |

## 39.1 The problem scripting solves

The WATCH loop (§31.1) costs a round trip per retry and cannot express "read this, decide, then write" without the client in the middle. `EVAL` moves the decision to the server: one round trip, atomic by construction, arbitrary logic. It is the reason Redis is used as a coordination primitive at all — every distributed lock, token bucket, and leaderboard-with-tiebreak library on your disk is a Lua script.

## 39.2 The execution contract

Six rules; each one is a build requirement.

1. **Atomic.** The script runs to completion with nothing interleaved — the same guarantee `EXEC` gives, for the same reason. In C it's the single thread; in DiceMe it is one engine message that happens to run a long time. Free, and dangerous: a slow script stalls every client, which is why §39.3 exists.
2. **Keys are declared.** `EVAL script numkeys key [key…] arg [arg…]`. The declared keys are what cluster mode slot-checks (§34.1) and what ACLs authorize (ch. 46). Outside cluster mode Redis does **not** enforce that the script only touches declared keys — it trusts you, exactly as §1.2 says it does. Reproduce that: check in cluster mode, trust outside it.
3. **Effects replication.** Since 7.0 the script text is never propagated. The writes the script performed are wrapped in `MULTI`/`EXEC` and propagated as ordinary commands, canonicalized per §20.4. Two `SPOP`s inside a script become two `SREM`s with the actual members. This is why chapter 30's propagation design carries the whole feature: if your eval layer already returns `(reply, effect)`, scripting is plumbing.
4. **`redis.call` vs `redis.pcall`.** `call` raises on error, aborting the script and returning that error to the client; `pcall` returns an error table the script can inspect. Errors from a script are prefixed so clients can tell them apart.
5. **The conversion table**, which you must implement exactly in both directions:

   | Lua → RESP | RESP → Lua |
   |---|---|
   | number → integer (**truncated**, `3.7` becomes `:3`) | integer → number |
   | string → bulk string | bulk string → string |
   | table → array, **stopping at the first `nil`** | array → table |
   | table with `ok` field → simple status | status → table with `ok` |
   | table with `err` field → error | error → table with `err` |
   | `false` → null | null → `false` |
   | `true` → `:1` | — |

   `redis.setresp(3)` switches the script's view to RESP3, adding `map`, `set`, `double`, and `big_number` table forms. Build RESP2 conversion first, then the RESP3 additions — your ch. 7 logical-reply layer makes the second one a switch statement.
6. **The API surface**: `redis.call`, `redis.pcall`, `redis.error_reply`, `redis.status_reply`, `redis.sha1hex`, `redis.log`, `redis.setresp`, `redis.breakpoint`/`redis.debug` (stubs are fine), `redis.set_repl` (selective effect replication), `redis.REPL_ALL|REPL_AOF|REPL_REPLICA|REPL_NONE`, and the `KEYS`/`ARGV` globals.

## 39.3 The busy-script problem

A script that loops forever holds the only execution context. Redis's answer: after `busy-reply-threshold` milliseconds (5000 default; the old name `lua-time-limit` is still accepted), the server starts answering **other** clients `-BUSY Redis is busy running a script...` and accepts only `SCRIPT KILL`, `FUNCTION KILL`, and `SHUTDOWN NOSAVE`. `SCRIPT KILL` succeeds only if the script has **not written yet** — after a write, killing it would leave a half-applied non-atomic effect, so the only exit is `SHUTDOWN NOSAVE`, losing the writes rather than corrupting the log.

Under the engine-goroutine model this is the one place your architecture is *worse* than C's: you cannot poll the socket mid-script from inside the engine. The build (§40.2) solves it with a Lua instruction hook that checks an atomic kill flag and an elapsed-time counter — the same shape as C's `luaMaskCountHook`.

## 39.4 Script cache versus Functions

**Scripts** are cached by SHA1: `SCRIPT LOAD` returns the digest, `EVALSHA <sha>` runs it, a miss returns `-NOSCRIPT`. The cache is *not* persisted and *not* replicated — client libraries handle `NOSCRIPT` by re-sending the body. `SCRIPT EXISTS`, `SCRIPT FLUSH [ASYNC|SYNC]`, `SCRIPT KILL` complete the surface. `EVAL_RO`/`EVALSHA_RO` refuse writes, which is what lets a replica serve them.

**Functions** (7.0) are the fix for everything the SHA1 cache got wrong. `FUNCTION LOAD` registers a named **library** whose functions declare flags (`no-writes`, `allow-oom`, `allow-stale`, `no-cluster`); `FCALL name numkeys …` invokes one. Libraries are **persisted in RDB and replicated** — they are part of the dataset, not per-connection state. `FUNCTION LIST|DELETE|FLUSH|DUMP|RESTORE|STATS|KILL` round it out, and `FUNCTION DUMP`'s payload rides in the RDB, which is why chapter 23 has to know about it.

## Self-check — Chapter 39

1. A script does `SPOP s` then `SET k <result>`. Exactly what bytes reach the replica, and why not the script text?
2. Why can `SCRIPT KILL` refuse? What is the only remaining exit, and what does it cost?
3. `return 3.7` and `return {1, nil, 3}` — what does the client receive for each?
4. Why are Functions replicated and persisted while the script cache is neither? Name the client-side bug that design removes.
5. Which two subsystems consume a script's declared key list, and what does each do with it?

---

# Chapter 40 — Build: scripting

> **Time:** 30 h · **Before you start:** Chapter 39.
>
> **You're done when:** `tests/ch40_script.txt` diffs clean, a runaway script is killable, and a script's writes reach a replica as canonical effects wrapped in MULTI/EXEC.

**Goal**: `EVAL`/`EVALSHA`(+`_RO`), the `SCRIPT` subcommands, the full `FUNCTION` surface, effects propagation, and the busy/kill machinery. One dependency joins the project here: a pure-Go Lua (`gopher-lua`) — the §6.1 exception, and the only one.

## 40.1 Order of work

1. **The bridge.** Embed the interpreter; register `redis.call`/`pcall` as Go functions that build a `[][]byte` argv and re-enter your dispatcher with a fake client (ch. 7's `NewDetachedClient`, now earning its third keep after AOF load and replication). Conversion both directions per §39.2's table, RESP2 only.
2. **The sandbox.** Remove `io`, `os`, `loadfile`, `dofile`, `require`, `package`, `print`, `collectgarbage`; freeze the global table so a script cannot create globals (real Redis errors `Script attempted to create global variable`); seed `math.random` deterministically per script invocation. Fuzz it: a script must never panic the server, and `go test -fuzz` on the script body is cheap insurance.
3. **`EVAL`/`EVALSHA`/`SCRIPT`.** SHA1 cache map, `-NOSCRIPT` error text byte-exact, `EVAL_RO`/`EVALSHA_RO` rejecting writes via the `CmdWrite` flag from ch. 7's table (reward collected again).
4. **Effects propagation.** Open a propagation batch before the script, collect every effect the nested dispatches produce, close it, and emit `MULTI … EXEC` if the batch is non-empty (a single-command batch propagates bare — match real Redis). `redis.set_repl` filters the batch. Verify on a live replica, not by reading your own code.
5. **Busy and kill.** A Lua instruction hook (`SetHook` every N instructions) checks elapsed time and an atomic kill flag. Past `busy-reply-threshold`, the engine must still answer other clients — so the script runs on its own goroutine while the engine loop stays alive answering `-BUSY` to everything except `SCRIPT KILL`/`FUNCTION KILL`/`SHUTDOWN NOSAVE`. **This is the one place a command does not run inside the engine goroutine**; the engine is *blocked on the script's completion channel* rather than executing, so atomicity holds — but write the invariant down in `NOTES.md`, because it is the single exception to §2.4 in the whole project.
6. **Functions.** Library registration and the `redis.register_function` callback, per-function flags, `FCALL`/`FCALL_RO`, the `FUNCTION` subcommands, and the RDB carriage: functions serialize into the RDB (opcode `0xF5`, §20.2) and load before the keyspace. `FUNCTION DUMP`/`RESTORE` reuse that serializer.
7. **Cluster and ACL hooks**: in cluster mode, all declared keys must hash to one slot (`-CROSSSLOT`); pass the declared keys to the ACL check once chapter 47 lands (leave the call site now).

## 40.2 Done-when

```bash
tests/harness/diff.sh tests/ch40_script.txt     # conversion matrix, error texts, NOSCRIPT, EVAL_RO refusals
redis-cli -p 7379 EVAL "return redis.call('SET', KEYS[1], ARGV[1])" 1 k v   # +OK
redis-cli -p 7379 EVAL "return {1, nil, 3}" 0                               # 1) (integer) 1   — stops at nil
redis-cli -p 7379 EVAL "return 3.7" 0                                        # (integer) 3
# effects: script does SPOP twice; replica's MONITOR shows MULTI, SREM <member>, SREM <member>, EXEC
# busy/kill: EVAL "while true do end" 0 in one client → other clients get -BUSY after 5s → SCRIPT KILL works
# busy/kill after write: script writes then loops → SCRIPT KILL refused (-UNKILLABLE), SHUTDOWN NOSAVE exits
# functions survive a restart: FUNCTION LOAD ; DEBUG RELOAD ; FCALL still resolves
# functions replicate: FUNCTION LOAD on master → FUNCTION LIST on replica shows it
go test -fuzz=FuzzEvalBody -fuzztime=60s ./core/script/   # no panics, no sandbox escape
```

## 40.3 Traps

- Building a second propagation path "just for scripts" — the §30.4 trap in a new costume. One effect stream, one producer, always.
- Forgetting that a nested `redis.call` can itself trigger expiry, eviction, or a blocked-client wakeup. Wakeups must be deferred to the end of the *script*, not the end of the inner command (same two-phase rule as §31.3, one level up).
- `KEYS`/`ARGV` must be fresh per invocation; a leaked global between invocations is a correctness bug and an information leak between clients.
- Truncation, not rounding: `return 3.7` is `:3` and `return -3.7` is `:-3`. Diff it.

---

# Chapter 41 — Streams

> **Time:** 3 h · **Before you start:** Chapter 40.
>
> **You're done when:** You can explain what the PEL is, who owns an entry after `XAUTOCLAIM`, and why `XADD` IDs are two numbers rather than one.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "Redis Streams" intro + the consumer-groups tutorial (`redis.io/docs/latest/develop/data-types/streams/`) | 1 h |
> | `t_stream.c`: the file's macro-node comment, `streamAppendItem`, `streamIteratorStart`/`Next`, `streamCreateConsumerGroup`, `streamReplyWithRange`, `xautoclaimCommand` | 1.5 h |
> | `rax.c` header comment (the compressed-radix-tree format) — you build this in ch. 17 | 30 min |

## 41.1 The data type

A stream is an **append-only log of field/value maps**, each stamped with a monotonic ID `<milliseconds>-<sequence>`. Two numbers, because a millisecond can hold many entries and IDs must be totally ordered and generated server-side without a clock guarantee: same ms → bump the sequence. `XADD s '*' field value …` auto-generates; `XADD s 5-*` fixes the ms and auto-sequences; an explicit ID must be strictly greater than the last one (`-ERR The ID specified in XADD is equal or smaller than the target stream top item`).

Storage is a **rax** keyed by the 128-bit ID, whose values are **listpack macro-nodes**: each node packs many entries sharing a field-name template, so a stream of uniform-shaped entries costs a few bytes per entry rather than a map per entry. This is §14.1's argument taken to its conclusion, and it is why `ds/rax` and `ds/listpack` are both prerequisites.

Beyond the entries, a stream carries metadata that the commands expose and the RDB must round-trip: `last-id`, `entries-added` (a monotonic counter, never decremented by deletion), `max-deleted-entry-id`, and the entry count. Deletion (`XDEL`) leaves **tombstones** in the macro-node rather than rewriting it, so `XLEN` and `entries-added` diverge deliberately.

## 41.2 Trimming

`XTRIM s MAXLEN 1000` and `XTRIM s MINID <id>`, plus the same options inline on `XADD`. The `~` form (`MAXLEN ~ 1000`) is **approximate**: trim only whole macro-nodes, stopping when the next node would have to be split. That turns an O(N) rewrite into O(1) amortized and is the default advice — `LIMIT n` caps the work per call. Exact trimming is available and expensive; both must exist, and the diff harness must distinguish them (`XLEN` after `~` trimming is ≥ the requested length, which is *correct*).

## 41.3 Reading: the two modes

**Fan-out (`XREAD`).** Every reader sees every entry: `XREAD COUNT 10 STREAMS s <id>` returns entries after `<id>`. The special ID `$` means "only entries added after this call", which only makes sense with `BLOCK`. `XREAD BLOCK 0 STREAMS s $` is the blocking form and plugs straight into chapter 33's ready-keys registry — `XADD` signals the key ready, exactly like `RPUSH` does for `BLPOP`.

**Consumer groups (`XREADGROUP`).** The durable variant, and the reason streams exist:

- `XGROUP CREATE s g <id|$> [MKSTREAM]` creates a group with a **last-delivered-ID** cursor.
- `XREADGROUP GROUP g c COUNT 10 STREAMS s >` delivers *new* entries to consumer `c`, advances the group cursor, and records each delivered entry in the **PEL** (Pending Entries List) — per-group and per-consumer, holding `(id, consumer, delivery-time, delivery-count)`.
- `XACK s g <id…>` removes entries from the PEL: the acknowledgement that work completed.
- `XREADGROUP … STREAMS s 0` re-delivers *this consumer's own* pending entries — crash recovery for a single consumer.
- `XPENDING s g` (summary) and `XPENDING s g IDLE ms start end count [consumer]` (extended) inspect the PEL.
- `XCLAIM s g newconsumer <min-idle-time> <id…>` transfers ownership of entries idle longer than the threshold — how a dead consumer's work is picked up. `XAUTOCLAIM s g newconsumer <min-idle> <start> [COUNT n]` is the cursor-driven bulk form (7.0), returning a next-cursor plus the list of entries it could not claim because they no longer exist.
- `XGROUP CREATECONSUMER|DELCONSUMER|SETID|DESTROY`, and `XINFO STREAM [FULL]|GROUPS|CONSUMERS` for introspection.

The delivery-count field is what lets a consumer build a dead-letter policy: claim, see `delivery-count > N`, `XACK` and route elsewhere. Redis provides the counter and no policy — §1.2 again.

## 41.4 What streams are not

Not exactly-once: `XREADGROUP` delivers, then your process may die before `XACK`, so the entry is redelivered. At-least-once with an idle-based reclaim is the honest description, and the PEL is the entire mechanism. Not a queue with priorities, not a partitioned log — one stream is one ordered sequence, and sharding is your key schema's problem (§34.1).

## Self-check — Chapter 41

1. Why is a stream ID two numbers? What breaks with one?
2. What does `entries-added` count that `XLEN` does not, and which command exposes the gap?
3. Trace an entry through: `XADD` → `XREADGROUP` → consumer dies → `XAUTOCLAIM` → `XACK`. Name the structure holding it at each step.
4. Why is `MAXLEN ~ 1000` the recommended form? What exactly does the `~` relax?
5. What does `XREADGROUP … STREAMS s 0` return, and when would a consumer send it?
6. Which chapter-33 mechanism does `XREAD BLOCK` reuse unchanged, and what signals it?

---

# Chapter 42 — Build: streams

> **Time:** 35 h · **Before you start:** Chapters 41, and a working chapter 33 (blocking) and chapter 17 (`ds/rax`).
>
> **You're done when:** `tests/ch42_stream.txt` diffs clean, a killed consumer's entries are reclaimed by `XAUTOCLAIM`, and a stream round-trips through RDB and replication with its groups and PELs intact.

**Goal**: the stream type on your rax + listpack, the full command surface, consumer groups with PEL semantics, blocking reads, and RDB/AOF/replication carriage.

## 42.1 Order of work

1. **The object.** `stream{rax *rax.Rax; last, maxDeleted streamID; added, length uint64; groups map[string]*group}`; macro-node encoding in listpack with the shared-field-template optimization (build the simple per-entry form first, add the template once the tests pass — the format is observable only through memory usage and RDB bytes).
2. **Non-group commands**: `XADD` (ID generation and validation, `NOMKSTREAM`, inline trimming), `XLEN`, `XRANGE`/`XREVRANGE` (with `-`/`+` and exclusive `(` bounds, `COUNT`), `XDEL`, `XTRIM` (`MAXLEN`/`MINID`, exact and `~` with `LIMIT`), `XSETID` (with `ENTRIESADDED`/`MAXDELETEDID`).
3. **`XREAD`**, then `XREAD BLOCK` through the chapter-33 registry: `XADD` calls `signalKeyAsReady`; the blocked client's resume point is the ID it asked from, so the wake handler must re-run the range query rather than hand over "the" entry (unlike `BLPOP`, several blocked readers all get the same entry — fan-out, not hand-off; test both).
4. **Groups**: the group struct (`lastDelivered`, `pel` keyed by ID, `consumers` each with their own PEL view and `seen-time`), `XGROUP` subcommands, `XREADGROUP` (`>` versus explicit IDs, `NOACK`), `XACK`, `XPENDING` both forms, `XCLAIM` (with `IDLE`/`TIME`/`RETRYCOUNT`/`FORCE`/`JUSTID`), `XAUTOCLAIM` (cursor plus the deleted-ID list), `XINFO STREAM|GROUPS|CONSUMERS|STREAM FULL`.
5. **Propagation** (§20.4, and the reason this chapter comes after ch. 30): `XADD s *` propagates with the **generated** ID, never `*`. `XREADGROUP` propagates `XCLAIM` entries so the replica's PEL matches. `XAUTOCLAIM` and `XCLAIM` propagate their resolved effects. Getting this wrong diverges the PEL silently, which is the stream-specific instance of the §28.7 bug list.
6. **Persistence**: RDB value type for streams (entries as raw listpack macro-nodes, then group and PEL metadata); AOF gets the propagated forms for free. `DEBUG RELOAD` must preserve every group, consumer, and pending entry.

## 42.2 Done-when

```bash
tests/harness/diff.sh tests/ch42_stream.txt   # ~600 lines: ID edge cases, ranges, trimming, groups, XINFO shapes
# blocking fan-out: three clients XREAD BLOCK 0 … $ ; one XADD ; all three wake with the same entry
# group hand-off: two consumers XREADGROUP > ; entries partitioned, never duplicated
# reclaim: consumer A reads 10, dies; XAUTOCLAIM by B after min-idle → B owns them, delivery-count incremented
# PEL survives: XREADGROUP ; DEBUG RELOAD ; XPENDING identical
# PEL replicates: same XPENDING output on master and replica after a group workload
# approximate trim: XADD ... MAXLEN ~ 1000 × 100k → XLEN >= 1000, and node count stays bounded
```

## 42.3 Traps

- `XADD s *` propagated verbatim gives the replica a different ID and permanent divergence. This is the §20.4 lesson's sharpest edge; test it first, not last.
- Tombstones: `XDEL` inside a macro-node must not renumber or shift IDs. `XRANGE` skips tombstones; `XINFO STREAM FULL` shows them.
- `XAUTOCLAIM` must return the IDs it *dropped* (entries deleted since being pended) in its third reply element — clients use it to clean their own bookkeeping, and it is easy to omit until the diff harness catches it.
- A group whose stream key is deleted disappears with it; `XGROUP CREATE` on a missing key errors unless `MKSTREAM`.

---

# Chapter 43 — Bitmaps, HyperLogLog, and geospatial

> **Time:** 3 h · **Before you start:** Chapter 42.
>
> **You're done when:** You can explain why all three of these command families store their data in an ordinary string or zset, and what that buys.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | `bitops.c`: `setbitCommand`, `bitcountCommand` (the `BIT`/`BYTE` range forms), `bitopCommand`, `bitfieldGeneric` | 45 min |
> | `hyperloglog.c`: the header comment — it is a summary of the paper — then `hllSparseToDense`, `hllAdd`, `hllCount` | 1 h |
> | **Flajolet, Fusy, Gandouet, Meunier, "HyperLogLog: the analysis of a near-optimal cardinality estimation algorithm"** (AofA 2007) — `algo.inria.fr/flajolet/Publications/FlFuGaMe07.pdf`; sections 1–4 | 1 h |
> | §15.9 of this book, re-read, plus `geohash.c:geohashEncode` and `geohash_helper.c` | 30 min |

Three command families, zero new storage types. That is the lesson binding them together, and it is the same lesson three times: **choose a representation you already have and map the query onto it.**

## 43.1 Bitmaps: a string you address by bit

`SETBIT k 7 1` grows the raw-encoded string to 1 byte and sets bit 7 (big-endian within the byte: bit 0 is the most significant). Everything follows: `GETBIT`, `BITCOUNT k [start end [BYTE|BIT]]`, `BITPOS k bit [start [end [BYTE|BIT]]]`, and `BITOP AND|OR|XOR|NOT dest src…` which operates on whole strings, zero-extending the shorter ones.

Two build notes that are easy to get wrong and that the diff harness will find: **`SETBIT` forces `raw` encoding and zero-fills the gap** (setting bit 10,000,000 allocates 1.25 MB of zeros — that is the documented memory bomb, gated by `proto-max-bulk-len`), and **`BITCOUNT`'s default range unit is `BYTE`** while negative indexes count from the end.

`BITFIELD` is the interesting one: a miniature typed VM over the byte array. `BITFIELD k SET u8 0 255 GET u8 0 INCRBY i5 100 -10 OVERFLOW SAT INCRBY i5 100 -10` executes a sequence of typed operations at bit offsets, with `u1…u63`/`i1…i64` types, `#`-prefixed offsets meaning "in units of the type width", and three overflow behaviours — `WRAP` (modular, default), `SAT` (clamp to the type's range), `FAIL` (return nil for that operation). `OVERFLOW` is a *mode switch* affecting subsequent operations in the same command, not an argument to one. `BITFIELD_RO` allows only `GET`, which is what makes it replica-safe.

## 43.2 HyperLogLog: cardinality in 12 KB

A HyperLogLog is a **plain Redis string** with a magic header, which is why `GET`/`SET` on a HLL key works and why `PFADD`'s output must be byte-compatible: other tools and other servers read these blobs.

The algorithm: hash the element, use the first 14 bits to pick one of **16384 registers**, count the leading zeros in the remaining bits, and keep the maximum leading-zero count per register. Many trailing zeros are unlikely, so a large maximum implies many distinct elements; averaging across registers (harmonically, with bias correction) gives a cardinality estimate with ~0.81% standard error. Registers are 6 bits, so 16384 × 6 / 8 = 12 KB — the dense encoding.

Redis adds two things the paper does not, and you must implement both:

- **The sparse encoding.** For small cardinalities most registers are zero, so the string instead holds a run-length-ish opcode stream (`ZERO`, `XZERO`, `VAL`) that costs a few dozen bytes. It converts to dense when it exceeds `hll-sparse-max-bytes` (3000) or when a register value exceeds 32. One-way, like every other encoding conversion (§8.2).
- **The cached cardinality.** The 16-byte header (`"HYLL"`, encoding byte, three reserved, 8-byte card) caches the last computed count with an invalidation bit in the MSB, because `PFCOUNT` is expensive and overwhelmingly repeated. `PFADD` that changes nothing must not invalidate it — that's also why `PFADD` returns 0/1 meaningfully.

`PFCOUNT` on multiple keys merges on the fly without storing; `PFMERGE` writes the register-wise maximum into a destination. `PFDEBUG` and `PFSELFTEST` exist for the test suite — implement `PFDEBUG GETREG` at minimum, because the real Redis stream of tests uses it.

## 43.3 Geospatial: a zset in disguise

§15.9 has the mechanism; this is the command surface. `GEOADD key [NX|XX] [CH] lng lat member …` stores a 52-bit Morton-interleaved geohash as the zset score. `GEOPOS` decodes back to coordinates (lossy at the 26th subdivision — ~0.6 m — so the diff harness needs a tolerance rule, one of the few legitimate entries in the §6.4 allowlist). `GEODIST` is haversine between two decoded members with a unit argument. `GEOHASH` returns the *standard* 11-character base-32 geohash string, which is a **different encoding** from the internal score — build the conversion, it is a common trip-up.

`GEOSEARCH key FROMMEMBER|FROMLONLAT BYRADIUS|BYBOX [ASC|DESC] [COUNT n [ANY]] [WITHCOORD] [WITHDIST] [WITHHASH]` is the modern query; `GEOSEARCHSTORE` writes results to a destination key (`STOREDIST` writes distances as scores instead of geohashes). The deprecated `GEORADIUS`/`GEORADIUSBYMEMBER` (+`_RO` variants) still ship in every client library, so build them as thin wrappers — and note their `STORE`/`STOREDIST` options make the non-`_RO` forms *write* commands, which matters for the ch. 7 flag table and for replica routing.

## Self-check — Chapter 43

1. Why does `SETBIT k 10000000 1` cost 1.25 MB? Which config is the only thing standing between that and an OOM?
2. `BITFIELD k OVERFLOW SAT INCRBY i8 0 100 INCRBY i8 0 100` — what are the two return values, and why?
3. What are the two HLL encodings, what triggers the conversion, and why is it one-way?
4. Why is a HyperLogLog a plain string rather than its own type? Name two consequences.
5. `GEOHASH` and the zset score are both geohashes. Why are they not the same bytes?
6. Which two geo commands are writes despite sounding like reads, and what makes them so?

---

# Chapter 44 — Build: bitmaps, HyperLogLog, and geospatial

> **Time:** 30 h · **Before you start:** Chapter 43.
>
> **You're done when:** A HyperLogLog written by DiceMe is counted correctly by real `redis-server` and vice versa, and `tests/ch44_*.txt` diff clean.

## 44.1 Order of work

1. **Bitmaps** (~8 h): `SETBIT`/`GETBIT` with raw-encoding forcing and zero-fill; `BITCOUNT`/`BITPOS` with both range units and negative indexes; `BITOP` including `NOT`'s single-source arity rule; then `BITFIELD`/`BITFIELD_RO` — write the operation parser first, the typed getter/setter second, the three overflow modes third, and property-test the typed accessors against a `math/big` model at every width from 1 to 64.
2. **HyperLogLog** (~14 h): the header, the dense encoding, the sparse encoding and its opcodes, the promotion rule, the cached-cardinality invalidation bit, `hllCount`'s estimator (implement the modern bias-corrected/loglog-beta form Redis uses, not the raw 2007 estimator — they disagree at low cardinality and the harness will show it), `PFADD`/`PFCOUNT`/`PFMERGE`/`PFDEBUG`/`PFSELFTEST`. Use **exactly Redis's hash** (its 64-bit MurmurHash variant) or the blobs will not interoperate.
3. **Geospatial** (~8 h): `geohashEncode`/`Decode`, the 9-neighbour computation, the score-range derivation per cell, haversine, then the command surface of §43.3 on top of your existing zset.

## 44.2 Done-when

```bash
tests/harness/diff.sh tests/ch44_bitops.txt      # SETBIT/GETBIT/BITCOUNT/BITPOS/BITOP/BITFIELD matrices
tests/harness/diff.sh tests/ch44_hll.txt         # PFADD/PFCOUNT/PFMERGE, sparse and dense
tests/harness/diff.sh tests/ch44_geo.txt         # with the documented coordinate tolerance rule only
# interop, the real acceptance test:
#   PFADD on dice → DUMP → RESTORE into real redis-server → PFCOUNT matches within 0
#   PFADD on real redis → same round trip into dice → PFCOUNT matches within 0
# accuracy: 1M distinct elements → PFCOUNT within 1% of truth; 100 elements → exact (sparse)
# BITFIELD property test: 100k random typed ops vs a big.Int model, all widths, all overflow modes
# GEOSEARCH: 10k points, BYRADIUS and BYBOX results identical to real redis (sorted, with tolerance)
```

## 44.3 Traps

- A different hash function makes your HLLs numerically plausible and completely non-interoperable. Port Redis's hash byte-for-byte and test the round trip *before* building the sparse encoding.
- `BITCOUNT k 0 0` counts the first **byte** by default but the first **bit** with `BIT`. Two characters, two different answers.
- `BITFIELD`'s `#` offsets multiply by the type width; mixing `#` and raw offsets in one command is legal.
- Geo distance comparisons at cell edges: the 9-cell block over-approximates, so *always* filter by exact haversine. Skipping the filter passes casual tests and fails at boundaries.

---

# Chapter 45 — Build: Sentinel

> Chapter 37 is the spec.
>
> **Time:** 25 h · **Before you start:** Chapters 37 and 33 (pub/sub) and a working chapter 30.
>
> **You're done when:** Three `dice-sentinel` processes detect a killed master, elect a leader, promote the freshest replica, reconfigure the others, and a client that only knows the sentinels keeps working across the failover.

**Goal**: a second binary, `dice-sentinel`, implementing the §37 protocol — and interoperating with real `redis-sentinel` where the protocol is observable.

## 45.1 Order of work

1. **Monitoring**: config (`sentinel monitor <name> <ip> <port> <quorum>`, `down-after-milliseconds`, `failover-timeout`, `parallel-syncs`), a connection per monitored instance, periodic `PING`/`INFO`, and the instance table (masters, their replicas discovered from `INFO replication`, other sentinels).
2. **Discovery**: publish to `__sentinel__:hello` on the monitored master every 2 s and subscribe to it — this is where chapter 33's pub/sub earns a second keep. Zero config beyond the master address.
3. **SDOWN → ODOWN**: per-sentinel timeout marking, then `SENTINEL is-master-down-by-addr` asking peers, quorum counting.
4. **Leader election**: `is-master-down-by-addr` doubles as the vote request (it carries a run-id and epoch); majority of *all known sentinels*, one vote per epoch, first-come-first-served.
5. **Failover state machine**: select target (exclude down/disconnected, then `replica-priority`, then highest replication offset, then lexicographic run-id) → `REPLICAOF NO ONE` → poll `INFO` until it reports master → `REPLICAOF <new>` to the others in batches of `parallel-syncs` → bump config epoch → announce via hello.
6. **The client side**: `SENTINEL get-master-addr-by-name`, `SENTINEL masters|replicas|sentinels|ckquorum|failover|reset`, and the ~50-line Go client that resolves the master through a sentinel and reconnects on `-READONLY`.

## 45.2 Done-when

```bash
tests/ch45_sentinel.sh up      # 1 master, 2 replicas, 3 sentinels
kill -9 <master>               # within down-after + failover time: a replica is master, others follow it
                               # the Go client's writes resume without restart
# split the sentinels 1|2: the lone sentinel sees ODOWN but never acts (no majority) — assert no promotion
# old master returns: receives REPLICAOF from a higher-epoch sentinel, demotes, full-resyncs
# interop: run one real redis-sentinel alongside two dice-sentinels against a dice master —
#   they discover each other over __sentinel__:hello and agree (this is the honest compatibility check)
```

## 45.3 Traps

- Detection uses *quorum*, acting uses *majority*. Two different numbers; conflating them lets a minority partition promote (§37.3).
- The promotion target must be re-validated *after* the election — the freshest replica at vote time may be down by execution time.
- `failover-timeout` governs retry and the refusal to start a second failover for the same master too soon; without it a flapping master produces a promotion storm.

---

# Chapter 46 — Security: AUTH, ACLs, and TLS

> **Time:** 2 h · **Before you start:** Chapter 45.
>
> **You're done when:** You can state what `+@all -@dangerous ~cache:* &news.*` grants, and where in the dispatch path each half of it is checked.
>
> ### 📚 Required reading
> | Read this | Time |
> |---|---|
> | Redis docs: "ACL" (`redis.io/docs/latest/operate/oss_and_stack/management/security/acl/`) and "Security" | 45 min |
> | `acl.c`: `ACLCheckAllPerm`, `ACLSetUser`, `ACLGetCommandPerm`, the category table; `commands/*.json` `acl_categories` fields | 45 min |
> | Redis docs: "TLS" (`redis.io/docs/latest/operate/oss_and_stack/management/security/encryption/`) | 20 min |

## 46.1 The pre-ACL world, still supported

`requirepass <pw>` plus `AUTH <pw>`, which in 6.0+ is *defined as* setting the password of the special `default` user. `protected-mode yes` (the default) refuses connections from non-loopback addresses when there is no password and no bind directive — the single change that ended a decade of unauthenticated Redis instances on the public internet. Unauthenticated clients get `-NOAUTH Authentication required.` on everything except `AUTH`, `HELLO`, `QUIT`, and `RESET`.

## 46.2 ACLs

A user is a set of rules applied in order, and every rule is one of four kinds:

- **Command rules**: `+@all`, `-@admin`, `+get`, `-set`, `+config|get` (subcommand granularity), `allcommands`/`nocommands`. Categories (`@read`, `@write`, `@keyspace`, `@admin`, `@dangerous`, `@fast`, `@slow`, `@pubsub`, `@scripting`, `@transaction`, …) come from each command's metadata — the command table you have been carrying since chapter 7 grows one more field.
- **Key patterns**: `~cache:*` (read and write), `%R~x:*` (read-only), `%W~y:*` (write-only), `allkeys`. Checked against the command's declared key positions, which is exactly why `COMMAND GETKEYS` (ch. 11) and the scripting key declaration (§39.2) exist.
- **Channel patterns**: `&news.*`, `allchannels` — checked by `SUBSCRIBE`/`PSUBSCRIBE`/`PUBLISH`.
- **Passwords and state**: `>password`, `<password`, `#<sha256hex>`, `nopass`, `on`/`off`, `reset`.

**Selectors** (7.0) let one user carry several independent permission sets: `(+get ~app1:*)` grants `GET` on `app1:*` in addition to the base rules — the fix for "this service needs read on X and write on Y" without two connections.

Surface: `ACL SETUSER|GETUSER|DELUSER|LIST|USERS|CAT|WHOAMI|GENPASS|LOG [RESET]|LOAD|SAVE|HELP`, plus `AUTH <user> <pass>` and `HELLO <ver> AUTH <user> <pass>`. Rules live either in the config file or in a separate `aclfile` (mutually exclusive; `ACL LOAD`/`SAVE` require the file). `ACL LOG` is the audit trail — every denial, with reason (`command`, `key`, `channel`, `auth`), and it is the first thing an operator asks for.

Error texts, byte-exact, because clients match them: `-NOPERM User <u> has no permissions to run the '<cmd>' command`, `-NOPERM No permissions to access a key`, `-NOPERM No permissions to access a channel`, `-WRONGPASS invalid username-password pair or user is disabled.`

## 46.3 TLS

`tls-port` (usually with `port 0`), `tls-cert-file`, `tls-key-file`, `tls-ca-cert-file`, `tls-auth-clients yes|no|optional`, and the separate switches `tls-replication`, `tls-cluster` for intra-server links. In Go this is `crypto/tls` wrapping the listener and dialer, which is genuinely a small change — *if* your connection layer is `net.Conn`-shaped rather than raw file descriptors. It is, because chapter 7 made it so; this is the delayed reward for retiring the kqueue path.

The reason it is required rather than a nice-to-have: every managed Redis and most corporate deployments mandate TLS, so a "replacement" that cannot speak it is not one.

## 46.4 The rest of the protection surface

`rename-command`-style command disabling (removing `FLUSHALL`, `DEBUG`, `KEYS` from the table by config); `CLIENT KILL` with its filter forms (`ID`, `ADDR`, `LADDR`, `TYPE`, `USER`, `SKIPME`, `MAXAGE`); `CLIENT PAUSE`/`UNPAUSE` (`WRITE`/`ALL` modes — under the engine model, simply stop dequeuing, and note it is also a failover primitive); `CLIENT NO-EVICT`/`NO-TOUCH`; never logging values.

## Self-check — Chapter 46

1. `AUTH <pw>` with no ACLs configured — what is actually happening under 6.0+ semantics?
2. Which three things does `+@all -@dangerous ~cache:* &news.*` deny, and at which point in the dispatch gate sequence is each denied?
3. Why does key-pattern checking depend on `COMMAND GETKEYS` working correctly? What breaks for a script?
4. What does `protected-mode` do, and what class of incident did it end?
5. Why are selectors more useful than creating a second user?

---

# Chapter 47 — Build: the security and admin surface

> **Time:** 25 h · **Before you start:** Chapter 46.
>
> **You're done when:** `tests/ch47_acl.txt` diffs clean, a TLS client connects and replicates over TLS, and every command's ACL categories match real Redis's `COMMAND INFO` output.

## 47.1 Order of work

1. **Command metadata** (~4 h): add `ACLCategories` and key-spec information to the ch. 7 command table for every command you have built. Generate it from real Redis's `commands/*.json` rather than typing it — a small Go generator reading those files into your table is 100 lines and eliminates a whole class of divergence. This also completes `COMMAND INFO`/`COMMAND DOCS`/`COMMAND GETKEYS` from chapter 11.
2. **Users and rules** (~10 h): the user struct, rule parsing (order matters — rules apply left to right), the four rule kinds, selectors, password hashing (SHA-256 hex), `default` user semantics, `requirepass` as an alias for setting it.
3. **The dispatch gate** (~3 h): slot it into the ch. 7 gate sequence at the position real Redis uses (after arity, before cluster redirect): authenticated? → command allowed? → keys allowed? → channels allowed? Each failure has its own error text. Scripts and the AOF/replication fake clients bypass the gate (the master is authoritative; `CLIENT_MASTER` and replay flags skip it) — a subtle rule that, done wrong, makes replicas refuse their own master's writes.
4. **`ACL` command surface + `ACL LOG`** (~4 h), `aclfile` load/save with atomic replacement (§21.2 — the third reuse).
5. **TLS** (~2 h): `crypto/tls` on the listener, the replica dialer, and the cluster bus; the config keys of §46.3; a done-when that runs the whole chapter-30 replication drill over TLS.
6. **Admin surface** (~2 h): `CLIENT KILL` filters, `CLIENT PAUSE`/`UNPAUSE`, `CLIENT NO-EVICT`/`NO-TOUCH`, `CLIENT INFO`, command disabling by config, `protected-mode`.

## 47.2 Done-when

```bash
tests/harness/diff.sh tests/ch47_acl.txt    # SETUSER parsing, GETUSER output shape, NOPERM texts, ACL LOG entries
redis-cli -p 7379 ACL SETUSER alice on '>pw' '~cache:*' +@read
redis-cli -p 7379 --user alice --pass pw GET cache:x    # ok
redis-cli -p 7379 --user alice --pass pw GET other      # NOPERM No permissions to access a key
redis-cli -p 7379 --user alice --pass pw SET cache:x 1  # NOPERM ... 'set' command
# categories match the oracle: for every command, COMMAND INFO's ACL categories == real redis's
# TLS: redis-cli --tls --cert ... connects; a TLS replica completes the ch.30 drill unchanged
# protected-mode: non-loopback connection with no password is refused with the exact message
# CLIENT PAUSE 3000 WRITE: reads continue, writes queue, everything drains after
```

## 47.3 Traps

- Applying ACL rules as a set instead of an ordered list. `+@all -get +@read` and `+@all +@read -get` grant different things.
- Forgetting to exempt the master link and the AOF loader from the gate — the failure looks like a replication bug, not a security bug, and costs an evening.
- `ACL GETUSER`'s reply shape changed across versions; diff it against your reference `redis-server`, not the docs.

---

# Chapter 48 — Build: the compatibility sweep

> The last chapter, and the one that turns "a Redis" into "*the* Redis".
>
> **Time:** 45 h · **Before you start:** Everything above.
>
> **You're done when:** Real client libraries' own test suites pass against DiceMe, and a documented majority of the real Redis TCL test suite passes unmodified.

**Goal**: close every remaining gap between your command table and real Redis's, then prove it with somebody else's tests instead of your own.

## 48.1 The remaining commands

- **RESP3 completion**: push frames for pub/sub delivery (the serializer exists since ch. 7; subscribers in RESP3 are *not* restricted, so lift the RESP2 mode restriction per protocol version), `HELLO` full reply shape, and **client-side caching**: `CLIENT TRACKING ON|OFF [REDIRECT id] [PREFIX p …] [BCAST] [OPTIN] [OPTOUT] [NOLOOP]`, `CLIENT TRACKINGINFO`, `CLIENT CACHING`, and invalidation messages fired from — say it — the keyspace choke point (§8.3). ~12 h, and the last dividend that choke point pays.
- **Hash field TTLs** (7.4): `HEXPIRE`/`HPEXPIRE`/`HEXPIREAT`/`HPEXPIREAT`/`HTTL`/`HPTTL`/`HEXPIRETIME`/`HPEXPIRETIME`/`HPERSIST`/`HGETEX`/`HGETDEL`. Per-field metadata plus a second expiry index — the chapter-9 contract one level down, including the replica rule (§9.4) and the propagation rule (§20.4: `HEXPIRE` propagates as `HPEXPIREAT`). ~10 h.
- **`OBJECT REFCOUNT`**, **`MEMORY USAGE|STATS|DOCTOR|PURGE`**, **`LOLWUT`**, **`TIME`**, **`SUBSTR`**, **`MOVE`**, **`SWAPDB`**, **`WAITAOF`**, **`FAILOVER [TO host port] [ABORT] [TIMEOUT ms]`** (pause writes → wait for the target replica to catch up → swap roles: `WAIT` + `CLIENT PAUSE` + `PSYNC` composed, and a satisfying capstone), **`SLOWLOG GET|LEN|RESET|HELP`**, **`LATENCY HISTORY|RESET|LATEST|DOCTOR`**. ~8 h.
- **The `DEBUG` surface the test suite depends on**: `OBJECT`, `SLEEP`, `SET-ACTIVE-EXPIRE`, `JMAP`, `QUICKLIST-PACKED-THRESHOLD`, `STRINGMATCH-LEN`, `LISTPACK`, `CHANGE-REPL-ID`, `RELOAD`, `LOADAOF`, `DIGEST`, `DIGEST-VALUE`, `OBJECT FREQ`. `DEBUG DIGEST`/`DIGEST-VALUE` must be *self-consistent* (the tests compare a master's digest to a replica's, never to a constant), so define it once and use it everywhere. ~5 h.
- **`INFO` field parity**: `commandstats`, `latencystats`, `errorstats`, `keyspace`, `replication` in both roles, `persistence` with every `rdb_*`/`aof_*` field, `everything`/`all` sections. Prometheus exporters parse these by name; a missing field is a broken dashboard. ~4 h.

## 48.2 The proof

Your own tests prove what you thought to test. These prove what you did not.

1. **The real Redis test suite.** `~/Code/Learning/redis/tests/` is TCL and points at a host/port. Run `tests/unit/type/*`, `tests/unit/{expire,keyspace,scan,scripting,pubsub,multi,bitops,hyperloglog,geo,acl,info}`, and `tests/integration/{replication*,aof*,rdb}` against DiceMe. You will not pass all of it — some tests reach into `DEBUG` internals that only make sense for C Redis. **Score it**: a `make compat` target that runs the suite and prints passed/failed/skipped per file, with every skip justified in one line in `COMPATIBILITY.md`. That file is the deliverable, and an honest 85% with reasons beats a claimed 100%.
2. **Real client libraries.** Point `go-redis`, `redis-py`, `node-redis`, and `jedis`'s own integration suites at DiceMe. They exercise `HELLO`, `COMMAND DOCS`, RESP3 negotiation, connection-pool edge cases, and cluster slot discovery — the parts no hand-written test covers, and the parts a *replacement* is judged on.
3. **The real tools.** `redis-cli` (including `--cluster`, `--bigkeys`, `--memkeys`, `--scan`, `--rdb`), `redis-benchmark`, `redis-check-rdb`, and `redis-check-aof` must all work against your server and your files.

## 48.3 Done-when

```bash
make compat                      # runs the real TCL suite; prints the scoreboard; writes COMPATIBILITY.md
# client libraries:
#   go-redis, redis-py, node-redis integration suites green against :7379 (RESP2 and RESP3)
redis-cli -p 7379 --bigkeys ; redis-cli -p 7379 --memkeys ; redis-cli -p 7379 --rdb /tmp/out.rdb
redis-check-rdb /tmp/out.rdb     # valid
redis-cli -3 -p 7379 HELLO 3     # full RESP3 map reply; then CLIENT TRACKING ON and watch invalidations
# hash field TTLs: HEXPIRE h 1 FIELDS 1 f ; wait ; HGET h f → nil ; HLEN reflects it; replica agrees
# FAILOVER: dice master + replica → FAILOVER → roles swap with zero lost acknowledged writes
```

## 48.4 What remains incompatible, deliberately

Write this section into your README, because "complete replacement" is a claim that has to name its own edges:

- **The modules ABI.** `MODULE LOAD` takes a compiled C shared object against Redis's internal ABI. A Go server cannot load one, and no amount of work changes that. Consequence: RedisJSON, RediSearch, RedisTimeSeries, RedisBloom and every third-party module are out of reach. If you need them, you need Redis. This is the one honest gap, and it is architectural rather than incomplete.
- **The cluster bus wire format.** Chapter 36 builds the cluster protocol's *logic* over your own encoding, so a DiceMe cluster is complete and a *mixed* DiceMe/Redis cluster is not. Clients never speak the bus, so this does not affect a client-facing replacement — but say so explicitly rather than letting someone discover it. Byte-compatible `clusterMsg` is a well-defined ~20 h project if you ever need it.
- **Internal-only `DEBUG` subcommands** that expose C data structures (`DEBUG SEGFAULT`, allocator internals, `DEBUG LISTPACK-ENTRIES`). Stub them with errors; note the skipped tests.

---

# Appendix A — Glossary

| Term | Meaning |
|---|---|
| **AOF** | Append-only file: log of every effective write in propagated (canonical) form; replayed at boot. |
| **ASK / ASKING** | Cluster redirect for a key mid-migration; one-shot, doesn't update the client's slot map; importer requires the ASKING prefix. |
| **backlog** | Master-side ring buffer of the recent replication stream; the partial-resync window. |
| **beforeSleep** | Per-loop-iteration completion hook: AOF flush, unblock handling, reply/feed writing. |
| **bio** | Background I/O threads: fsync, close, lazy-free. Ownership handoff, never sharing. |
| **choke point** | This book's name for the db.c keyspace API where all cross-cutting hooks attach (dirty, watch, ready, notify, propagate). |
| **configEpoch / currentEpoch** | Cluster's monotonic conflict-resolution counters; higher epoch wins slot-ownership disputes. |
| **COW** | Copy-on-write: kernel duplicates memory pages only when the forking parent writes; how BGSAVE snapshots free in C. |
| **CROSSSLOT** | Error for multi-key ops spanning cluster slots; hash tags are the escape hatch. |
| **dirty counter** | Count of effective writes since last save; drives save points. |
| **diskless sync** | Full resync streaming the RDB straight to replica sockets, EOF-delimited. |
| **embstr** | String encoding ≤44 bytes: object header and payload in one allocation. |
| **encoding** | Physical representation of a value (listpack, skiplist…); private, vs *type* (public contract). |
| **effect propagation** | Replicating/logging what a command *did* (deterministic), not what was typed: SPOP→SREM, EXPIRE→PEXPIREAT. |
| **eviction pool** | 16-slot best-candidates-so-far array, sorted by idle/LFU rank, persisting across evictions. |
| **fake client** | A client struct with no socket, driving execution for AOF replay, replication apply, scripts. |
| **FAIL / PFAIL** | Cluster failure states: possibly-failed (one node's timeout) vs convicted (majority of masters agree). |
| **FULLRESYNC / CONTINUE** | PSYNC's two answers: RDB transfer + stream, or backlog replay from offset. |
| **hash tag** | `{…}` in a key: only that part hashes to a slot; the multi-key co-location tool. |
| **incremental rehash** | Two-table dict migration a bucket at a time, on ops + cron slices. |
| **keyspace notification** | Optional pub/sub event per keyspace change (`__keyevent@0__:expired`…). |
| **lazy free / UNLINK** | O(1) unlink from dict + background reclamation of the value. |
| **LFU** | Frequency eviction: 8-bit probabilistic (Morris) counter + minute-decay in the 24-bit lru field. |
| **listpack** | Contiguous-buffer small-collection encoding; successor of ziplist (no cascade updates). |
| **LRU clock** | 24-bit seconds counter cached server-wide; per-object stamp for idle-time estimation. |
| **maxmemory policy** | What to do when full: noeviction, or {allkeys,volatile}×{lru,lfu,random}, volatile-ttl. |
| **MOVED** | Permanent cluster redirect: slot lives elsewhere; client updates its map. |
| **MULTI dirty flags** | DIRTY_EXEC (queue-time validation failed → EXECABORT), DIRTY_CAS (watched key touched → null EXEC). |
| **ODOWN / SDOWN** | Sentinel: objectively down (quorum agrees) vs subjectively down (one sentinel's timeout). |
| **PSYNC** | The replication handshake verb carrying (replid, offset). |
| **quicklist** | Linked list of listpacks; the large-list encoding. |
| **RDB** | Point-in-time binary snapshot format (`REDIS0011` … CRC64). |
| **ready_keys** | Server-level queue of keys made ready this command, processed after it completes, to wake blocked clients. |
| **replid / replid2** | 40-hex history identifiers; replid2 = previous history kept briefly after promotion for sibling partial-syncs. |
| **replication offset** | Byte position in the canonical command stream; the coordinate of all replication bookkeeping. |
| **RESP2/RESP3** | The wire protocol; 3 adds typed replies + push frames via HELLO. |
| **save point** | `save M N`: BGSAVE when ≥N changes and ≥M seconds. |
| **SCAN guarantee** | Keys present throughout a scan are returned ≥once; duplicates possible; churned keys unspecified. |
| **skiplist** | Probabilistic ordered structure; zset's large encoding; spans give O(log n) rank. |
| **slot** | 1/16384 of the cluster keyspace: CRC16(key) mod 16384. |
| **WAIT** | Block until N replicas ack an offset — synchronous replication on demand, not consensus. |
| **WATCH** | Optimistic lock: EXEC aborts (null) if a watched key was written (incl. expired/evicted) since WATCH. |
| **ACL selector** | An extra, independent permission set attached to one user: `(+get ~app1:*)`. |
| **consumer group** | Stream reader group with a shared cursor and a per-consumer pending list; the at-least-once delivery mechanism. |
| **effects batch** | The MULTI/EXEC-wrapped set of writes a script or transaction propagates instead of itself. |
| **HLL sparse/dense** | HyperLogLog's two register encodings: opcode-compressed for small cardinalities, 16384×6 bits (12 KB) after promotion. |
| **LZF** | The compression codec inside RDB string encoding; without it you cannot read a default-configured real `dump.rdb`. |
| **PEL** | Pending Entries List: stream entries delivered to a consumer group but not yet `XACK`ed, with owner, idle time and delivery count. |
| **push frame** | RESP3's server-initiated message type (`>`): pub/sub delivery and cache invalidation, which RESP2 fakes with arrays. |
| **rax** | Compressed radix tree; ordered index for stream IDs and tracking prefixes. |
| **tombstone** | A deleted stream entry's slot, left in place so IDs never shift; why `XLEN` and `entries-added` diverge. |
| **tracking (CSC)** | Client-side caching: the server remembers which keys a client read and pushes invalidations when they change. |
| **ziplist cascade update** | The O(N²) re-encoding chain in listpack's predecessor; the reason listpack exists. |

---

# Appendix B — Command surface, chapter by chapter

The contract for "done": every command listed, semantics diff-clean vs real Redis (modulo the documented nondeterminism allowlist).

### ch. 7
`PING ECHO QUIT HELLO(2|3)` · RESP2 **and** RESP3 rendering of every reply shape · dispatch errors (unknown cmd, arity) · inline commands · `DEBUG SLEEP` · carried-over Phase-1 commands keep working

### ch. 11
Strings: `SET(NX|XX|GET|EX|PX|EXAT|PXAT|KEEPTTL) SETNX SETEX PSETEX GETSET GET GETDEL GETEX APPEND STRLEN SETRANGE GETRANGE INCR DECR INCRBY DECRBY INCRBYFLOAT MSET MSETNX MGET`
Keys: `DEL UNLINK EXISTS TYPE TOUCH COPY RENAME RENAMENX KEYS SCAN(MATCH|COUNT|TYPE) RANDOMKEY TTL PTTL EXPIRE PEXPIRE EXPIREAT PEXPIREAT(NX|XX|GT|LT) PERSIST EXPIRETIME PEXPIRETIME DBSIZE FLUSHDB FLUSHALL SELECT`
Keys, continued: `MOVE SWAPDB EXPIRETIME PEXPIRETIME` · `SELECT` across all 16 databases
Server: `COMMAND(|COUNT|INFO|DOCS|LIST|GETKEYS) CONFIG(GET|SET|RESETSTAT) INFO(+commandstats|errorstats|latencystats) TIME CLIENT(ID|GETNAME|SETNAME|LIST|INFO) OBJECT(ENCODING|IDLETIME|FREQ|HELP) DEBUG(OBJECT|SLEEP|SET-ACTIVE-EXPIRE|JMAP|STRINGMATCH-LEN)` · legacy alias `SUBSTR`(=GETRANGE)

### ch. 19
Lists: `LPUSH RPUSH LPUSHX RPUSHX LPOP RPOP LMPOP LLEN LRANGE LINDEX LSET LINSERT LREM LTRIM LMOVE RPOPLPUSH LPOS`
Hashes: `HSET HSETNX HGET HMGET HGETALL HDEL HLEN HEXISTS HKEYS HVALS HINCRBY HINCRBYFLOAT HSTRLEN HRANDFIELD HSCAN`
Sets: `SADD SREM SISMEMBER SMISMEMBER SMEMBERS SCARD SPOP SRANDMEMBER SMOVE SINTER SINTERCARD SUNION SDIFF SINTERSTORE SUNIONSTORE SDIFFSTORE SSCAN`
ZSets: `ZADD ZREM ZSCORE ZMSCORE ZCARD ZINCRBY ZRANK ZREVRANK ZRANGE ZRANGESTORE ZRANGEBYSCORE ZREVRANGEBYSCORE ZRANGEBYLEX ZREVRANGEBYLEX ZCOUNT ZLEXCOUNT ZPOPMIN ZPOPMAX ZMPOP ZRANDMEMBER ZREMRANGEBYRANK ZREMRANGEBYSCORE ZREMRANGEBYLEX ZSCAN ZUNION ZINTER ZDIFF ZUNIONSTORE ZINTERSTORE ZDIFFSTORE ZINTERCARD`
Cross-type: `SORT SORT_RO` (BY/GET pattern deref, ALPHA, LIMIT, STORE) · `LCS`
Deprecated aliases clients still send (each is a one-liner over the modern form — add them or your diff harness fails on real client libraries): `HMSET` (=HSET), `BRPOPLPUSH` (=BLMOVE RIGHT LEFT), `RPOPLPUSH` (=LMOVE RIGHT LEFT), `SETNX`/`SETEX`/`PSETEX`/`GETSET`, `SUBSTR` (=GETRANGE), `ZRANGEBYSCORE`-family (=ZRANGE forms)

### ch. 23
`SAVE BGSAVE BGREWRITEAOF LASTSAVE SHUTDOWN(NOSAVE|SAVE)` · `DUMP RESTORE(REPLACE|ABSTTL|IDLETIME|FREQ)` · `CONFIG SET appendfsync|save|appendonly|rdbcompression` · `-LOADING` behavior · `DEBUG RELOAD|LOADAOF` · LZF both directions · RDB interop with real `redis-server` both ways

### ch. 26
`CONFIG SET maxmemory|maxmemory-policy|maxmemory-samples|maxmemory-clients` · `-OOM` on writes under noeviction · `OBJECT FREQ` real under lfu · `INFO stats: evicted_keys, evicted_clients` · `MEMORY USAGE(SAMPLES)` · client eviction · `DEBUG RECOUNT-MEMORY` (yours)

### ch. 30
`REPLICAOF/SLAVEOF(host port|NO ONE) PSYNC REPLCONF(listening-port|capa|ACK|GETACK) WAIT ROLE` · `-READONLY` · `INFO replication` complete both roles · `DEBUG DIGEST` (yours)

### ch. 33
`MULTI EXEC DISCARD WATCH UNWATCH RESET` · `BLPOP BRPOP BLMOVE BRPOPLPUSH BLMPOP BZPOPMIN BZPOPMAX BZMPOP WAIT` · `SUBSCRIBE UNSUBSCRIBE PSUBSCRIBE PUNSUBSCRIBE PUBLISH PUBSUB(CHANNELS|NUMSUB|NUMPAT)` in both RESP2 (restricted mode) and RESP3 (push frames) · `CONFIG SET notify-keyspace-events`

### ch. 36
`CLUSTER(MYID|INFO|SHARDS|SLOTS|KEYSLOT|MEET|FORGET|RESET|BUMPEPOCH|SETSLOT|GETKEYSINSLOT|COUNTKEYSINSLOT|NODES|LINKS|REPLICAS|REPLICATE|FAILOVER|SET-CONFIG-EPOCH|SLAVES)` · `MIGRATE ASKING READONLY READWRITE` · `-MOVED -ASK -CROSSSLOT -TRYAGAIN -CLUSTERDOWN` · `SSUBSCRIBE SUNSUBSCRIBE SPUBLISH PUBSUB(SHARDCHANNELS|SHARDNUMSUB)`

### ch. 40
`EVAL EVAL_RO EVALSHA EVALSHA_RO` · `SCRIPT(LOAD|EXISTS|FLUSH|KILL)` · `FUNCTION(LOAD|LIST|DELETE|FLUSH|DUMP|RESTORE|STATS|KILL) FCALL FCALL_RO` · `-NOSCRIPT -BUSY -UNKILLABLE` · effects propagation wrapped in MULTI/EXEC

### ch. 42
`XADD XLEN XRANGE XREVRANGE XDEL XTRIM XSETID XREAD(BLOCK)` · `XGROUP(CREATE|CREATECONSUMER|DELCONSUMER|DESTROY|SETID) XREADGROUP XACK XPENDING XCLAIM XAUTOCLAIM XINFO(STREAM|STREAM FULL|GROUPS|CONSUMERS)` · stream RDB carriage including groups and PELs

### ch. 44
`SETBIT GETBIT BITCOUNT BITPOS BITOP BITFIELD BITFIELD_RO` · `PFADD PFCOUNT PFMERGE PFDEBUG PFSELFTEST` · `GEOADD GEOPOS GEODIST GEOHASH GEOSEARCH GEOSEARCHSTORE GEORADIUS GEORADIUS_RO GEORADIUSBYMEMBER GEORADIUSBYMEMBER_RO`

### ch. 45
`SENTINEL(masters|master|replicas|sentinels|get-master-addr-by-name|is-master-down-by-addr|reset|failover|ckquorum|monitor|remove|set|flushconfig)` in the `dice-sentinel` binary · `__sentinel__:hello` discovery

### ch. 47
`AUTH(pass|user pass) HELLO(AUTH)` · `ACL(SETUSER|GETUSER|DELUSER|LIST|USERS|CAT|WHOAMI|GENPASS|LOG|LOAD|SAVE|HELP)` · `-NOAUTH -NOPERM -WRONGPASS` · TLS listener, replica dialer and cluster bus · `CLIENT(KILL|PAUSE|UNPAUSE|NO-EVICT|NO-TOUCH)` · `protected-mode`, command disabling

### ch. 48
`CLIENT(TRACKING|TRACKINGINFO|CACHING)` + RESP3 invalidation push · `HEXPIRE HPEXPIRE HEXPIREAT HPEXPIREAT HTTL HPTTL HEXPIRETIME HPEXPIRETIME HPERSIST HGETEX HGETDEL` · `OBJECT REFCOUNT MEMORY(USAGE|STATS|DOCTOR|PURGE) LOLWUT SUBSTR WAITAOF FAILOVER` · `SLOWLOG(GET|LEN|RESET|HELP) LATENCY(HISTORY|RESET|LATEST|DOCTOR)` · `DEBUG(JMAP|QUICKLIST-PACKED-THRESHOLD|LISTPACK|CHANGE-REPL-ID|DIGEST|DIGEST-VALUE|LOADAOF)` · full `INFO` section parity

---

# Appendix C — Source index (concept → where in the real code)

All in `~/Code/Learning/redis/src` @ `d22066d09`. Search by function name.

| Concept | File : function |
|---|---|
| event loop core | `ae.c:aeMain`, `aeProcessEvents`; `ae_kqueue.c` |
| loop hooks | `server.c:beforeSleep`, `server.c:serverCron` |
| boot | `server.c:main`, `initServer`, `initListeners` |
| the three structs | `server.h:redisServer`, `redisDb`, `client` |
| accept/read/parse | `networking.c:createClient`, `readQueryFromClient`, `processMultibulkBuffer`, `processInlineBuffer` |
| replies/output buffers | `networking.c:addReply*`, `writeToClient`, `handleClientsWithPendingWrites` |
| dispatch gates | `server.c:processCommand` |
| execution + propagation trigger | `server.c:call`, `alsoPropagate`, `propagateNow` |
| command metadata | `commands/*.json` → `commands.def` |
| dict + rehash | `dict.c:dictAddRaw`, `dictRehash`, `_dictRehashStepIfNeeded` |
| SCAN cursor | `dict.c:dictScan` (+ the comment essay) |
| 8.x keyspace | `kvstore.c` (per-slot dicts), `ebuckets.c`/`estore.c` (TTL index), `entry.c` (kvobj) |
| object model | `object.c:createObject`, `tryObjectEncoding`; `server.h:robj` |
| keyspace API / choke point | `db.c:lookupKeyReadWithFlags`, `lookupKeyWrite`, `setKey`, `dbGenericDelete`, `scanGenericCommand` |
| lazy expiry | `db.c:expireIfNeeded` (the replica rule in its comment) |
| active expiry | `expire.c:activeExpireCycle`, `expireGenericCommand` |
| eviction | `evict.c:performEvictions`, `evictionPoolPopulate`, `LFULogIncr`, `LFUDecrAndReturn` |
| strings | `t_string.c:setGenericCommand`, `incrDecrCommand`, `appendCommand` |
| lists | `t_list.c:pushGenericCommand`; `quicklist.c:quicklistPush`, node split/merge; `listpack.c:lpInsert` |
| hashes | `t_hash.c:hashTypeSet` + conversion fns |
| sets | `t_set.c:setTypeAdd` (encoding ladder); `intset.c` |
| zsets | `t_zset.c:zslInsert` (spans), `zsetAdd` (flags matrix), `genericZrangebyscoreCommand` |
| RDB write/read | `rdb.c:rdbSaveRio`, `rdbSaveLen`, `rdbSaveObject`, `rdbLoadRio`; opcodes `rdb.h` |
| fork child lifecycle | `rdb.c:rdbSaveBackground`, `backgroundSaveDoneHandler`; `childinfo.c` |
| AOF feed/flush/fsync | `aof.c:feedAppendOnlyFile`, `flushAppendOnlyFile` (everysec stall branch) |
| AOF rewrite + manifest | `aof.c:rewriteAppendOnlyFileBackground` + file-header comment |
| AOF load | `aof.c:loadSingleAppendOnlyFile` (fake client) |
| bg threads | `bio.c` (whole file); `lazyfree.c` |
| io-threads (8.x) | `iothread.c:assignClientToIOThread`, `enqueuePendingClientsToMainThread`, `isClientMustHandledByMainThread`, `prefetchIOThreadCommands` (6.0–7.x lived in `networking.c` as a batch+barrier — see §2.3) |
| repl master side | `replication.c:syncCommand`, `masterTryPartialResynchronization`, `replicationFeedSlaves`, `feedReplicationBuffer` |
| repl replica side | `replication.c:syncWithMaster`, `readSyncBulkPayload`, `replicationCron` |
| WAIT | `replication.c:waitCommand` |
| MULTI/WATCH | `multi.c` (whole), `touchWatchedKey` |
| blocking | `blocked.c:blockForKeys`, `handleClientsBlockedOnKeys`, `unblockClient` |
| pub/sub + notify | `pubsub.c:pubsubPublishMessage`; `notify.c:notifyKeyspaceEvent` |
| cluster redirects | `cluster.c:getNodeByQuery` |
| cluster bus/gossip | `cluster_legacy.c:clusterProcessPacket`, `clusterSendPing`, `markNodeAsFailingIfNeeded` |
| cluster failover | `cluster_legacy.c:clusterHandleSlaveFailover`, `clusterHandleConfigEpochCollision` |
| sentinel | `sentinel.c:sentinelCheckObjectivelyDown`, `sentinelStartFailover` |
| scripting | `script.c`, `script_lua.c`, `functions.c` |
| streams | `t_stream.c`; `rax.c` |
| geospatial | `geo.c:geoaddCommand`, `geoSearchCommand`; `geohash.c:geohashEncode`; `geohash_helper.c` (neighbors, radius→cells) |
| bitmaps + BITFIELD | `bitops.c:bitcountCommand`, `bitfieldGeneric` |
| HyperLogLog | `hyperloglog.c` (dense/sparse registers; the header comment is a paper summary) |
| SORT | `sort.c:sortCommandGeneric` (BY/GET pattern deref) |
| hash field TTLs | `t_hash.c` HFE sections (`OBJ_ENCODING_LISTPACKEX`, ebuckets per hash) |
| multi-key pops | `t_list.c:lmpopCommand`, `blmpopCommand`; `t_zset.c:zmpopCommand` |
| sharded pub/sub | `pubsub.c:spublishCommand`, `ssubscribeCommand` |
| CRC | `crc16.c` (cluster slots), `crc64.c` (RDB) |
| glob matching | `util.c:stringmatchlen` |
| radix tree | `rax.c` (whole file; the header comment is the format spec) |
| RDB compression | `lzf_c.c` / `lzf_d.c` (compress and decompress, ~250 lines together) |
| DUMP / RESTORE | `cluster.c:dumpCommand`, `restoreCommand` (RDB value + version footer + CRC64) |
| ACLs | `acl.c:ACLCheckAllPerm`, `ACLSetUser`, `ACLGetCommandPerm`, `ACLAddLogEntry`; categories in `commands/*.json` |
| TLS | `tls.c` (the connection-type abstraction it plugs into is `connection.c`) |
| RESP3 / protocol version | `networking.c:addReplyMap`, `addReplyPush`, `helloCommand` |
| client tracking (CSC) | `tracking.c:trackingRememberKeys`, `trackingInvalidateKey` |
| client eviction | `networking.c:evictClients`, `updateClientMemUsageAndBucket` |
| command metadata | `commands/*.json` (arity, flags, key specs, ACL categories) → `commands.def` |
| SLOWLOG / LATENCY | `slowlog.c`; `latency.c:latencyAddSample` |
| protections | `config.c` (registry), `networking.c` (output buffer limits in `checkClientOutputBufferLimits`) |

---

# Appendix D — Reading list, ranked

Redis has less canonical literature than a consensus system does — much of its design was argued on a blog rather than at a conference, and that blog *is* the primary source. But the papers behind the individual mechanisms are real, and several will change what you build. Papers are marked **[P]**.

## Tier 1 — the primary sources, scheduled inside the chapters

1. **Redis official docs — the six specs.** Each is *the* specification for a build chapter, and each is about an hour:
   RESP `redis.io/topics/protocol` (ch. 4) · Persistence `redis.io/topics/persistence` (ch. 20) · Replication `redis.io/topics/replication` (ch. 27) · Cluster spec `redis.io/topics/cluster-spec` (ch. 34) · Transactions `redis.io/topics/transactions` (ch. 31) · Eviction/LRU `redis.io/topics/lru-cache` (ch. 24).
2. **Redis docs, per feature as you reach it:** memory optimization `redis.io/topics/memory-optimization` · keyspace notifications `redis.io/topics/notifications` · pub/sub `redis.io/topics/pubsub` · streams `redis.io/topics/streams-intro` · latency `redis.io/topics/latency` · Sentinel `redis.io/topics/sentinel` · security `redis.io/topics/security` · and every command page at `redis.io/commands/<name>` — the complexity line and the RETURN section are the contract your diff harness enforces.
3. **The four best in-code essays**, worth more than most blog posts: `dict.c`'s `dictScan` comment (the reverse-binary cursor) · `aof.c`'s file header (why the manifest exists) · `replication.c`'s file header · `hyperloglog.c`'s header (a paper summary in comments).
4. **antirez's blog** — `antirez.com`, and the pre-2018 archive. Redis's design rationale lives here, not in papers: "Redis persistence demystified" · "Random notes on improving the Redis LRU algorithm" (the sampled-LRU scatter plots, ch. 24) · "Streams: a new general purpose data structure in Redis" · "Redis Sentinel and CAP" (the consistency position, stated by the author) · "Clarifications about Redis and Memcached".
5. **[P] Pugh, "Skip Lists: A Probabilistic Alternative to Balanced Trees"** (CACM 1990) — `epaperpress.com/sortsearch/download/skiplist.pdf`. Ten pages, and you implement it in ch. 17.
6. **[P] Aumasson & Bernstein, "SipHash: a fast short-input PRF"** (INDOCRYPT 2012) — `131002.net/siphash/siphash.pdf`. Why the keyspace hash is a keyed PRF and not something faster (ch. 15).

## Tier 2 — the mechanism papers you implement

Each of these changes code you write. They are scheduled inside their chapters.

7. **[P] Pillai et al., "All File Systems Are Not Created Equal"** (OSDI 2014) — `usenix.org/system/files/conference/osdi14/osdi14-paper-pillai.pdf`. Which crash-atomicity properties you may actually assume; it finds these bugs in SQLite, Git and PostgreSQL, so assume they are in yours. Read with ch. 21.
8. **[P] Rebello et al., "Can Applications Recover from fsync Failures?"** (USENIX ATC 2020) — `usenix.org/conference/atc20/presentation/rebello`. A failed fsync can mark pages clean, so the retry lies to you. Read with ch. 21, then the PostgreSQL fsyncgate thread at `wiki.postgresql.org/wiki/Fsync_Errors`.
9. **[P] Flajolet, Fusy, Gandouet, Meunier, "HyperLogLog"** (AofA 2007) — `algo.inria.fr/flajolet/Publications/FlFuGaMe07.pdf`. You implement it in ch. 44.
10. **[P] Morris, "Counting Large Numbers of Events in Small Registers"** (CACM 1978) — two pages; the LFU counter in ch. 24 *is* this. Overview: `en.wikipedia.org/wiki/Approximate_counting_algorithm`.
11. **[P] Das, Gupta, Motivala, "SWIM"** (DSN 2002) — `cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf`. Cluster's PFAIL/FAIL gossip in its original form; §3–4 are the protocol. Read with ch. 34.
12. **Geohash and Z-order** — `en.wikipedia.org/wiki/Geohash`, `en.wikipedia.org/wiki/Z-order_curve`. The §15.9 mechanism you build in ch. 44.

## Tier 3 — background, none of it on the critical path

Nothing here changes a line of your code. It is here so you know it exists; read it after ch. 48, or never, without guilt.

13. **[P] Yang, Yue, Rashmi, "A large-scale analysis of hundreds of in-memory key-value cache clusters at Twitter"** (OSDI 2020) — real workloads, and several findings that contradict cache folklore. The most interesting paper on this list; it will not change what you build, only what you expect.
14. **[P] Atikoglu et al., "Workload Analysis of a Large-Scale Key-Value Store"** (SIGMETRICS 2012) and **[P] Nishtala et al., "Scaling Memcache at Facebook"** (NSDI 2013) — the workload Redis's design answers, and the operations problems it does not.
15. **[P] Evans, jemalloc** (BSDCan 2006) — why `used_memory` and `used_memory_rss` diverge.
16. **[P] Rosenblum & Ousterhout, LFS** (SOSP 1991) — your AOF is a log-structured store and `BGREWRITEAOF` is segment cleaning.
17. **[P] Megiddo & Modha, ARC** (FAST 2003) and **[P] Einziger et al., TinyLFU** — better eviction than Redis ships.
18. **Kleppmann, *Designing Data-Intensive Applications***, ch. 5 and 9 — where Redis's replication sits on the consistency map. **[P] Ongaro & Ousterhout, Raft** — what an acknowledged write costs when you refuse to lose it.
19. **Valkey**, **DragonflyDB**, **KeyDB** — the forks and the road not taken in §2.4.

**Anti-list:** build-your-own-Redis tutorials. You are past them by ch. 11, and their shortcuts — no incremental parse, no encodings, no propagation discipline — are precisely the bugs the audit in the front matter is about.

---

# Appendix E — Self-test bank

All chapter self-checks collected, plus integration questions. Answer out loud, from memory; per-chapter lists live in their chapters — these are the cross-cutting finals:

1. Trace `SET k v EX 10` from socket bytes to disk bytes: name every buffer it rests in and every hook it fires, in order. (§13.1 — do it from memory.)
2. A key with TTL 5 s is written on the master. List *every* place that fact is recorded, on master, replica, AOF, and — after failover — the new master.
3. Enumerate everything that can delete a key (7+ paths) and verify each fires the same choke-point hooks. Which one did your audit-era code silently skip?
4. Why does the same 24-bit field serve LRU and LFU? What breaks if you switch policy on a live dataset (and why is that acceptable)?
5. Your snapshot goroutine and a `HSET` race. Walk both the buggy interleaving (in-place mutation) and the correct one (copy-on-write) at the pointer level.
6. Partial resync is granted — list the exact state on both sides justifying it. Now the backlog wrapped 1 byte past the replica's offset — what happens, precisely?
7. Why can a MULTI batch never interleave with a BLPOP wakeup, in both C Redis and your engine model — and what *different* mechanism enforces it in each?
8. `redis-cli -c` gets MOVED then ASK for the same key within a second. Reconstruct the cluster state that produces this.
9. For each: acked-write loss possible? (a) everysec crash (b) always crash (c) master dies, replica promoted (d) WAIT 1 returned 1, then master dies (e) cluster minority partition heals. Justify each in one sentence.
10. Rank by production frequency and explain: full-resync loop, output-buffer OOM, KEYS-in-prod stall, unbounded expiry backlog, split-brain write loss.
11. A Lua script does `SPOP s` then `XADD st * member <result>`. Write out, byte for byte, what the replica receives — and name the two canonicalizations involved.
12. A consumer reads 10 entries, dies, and is replaced. Name every structure that has to change for those 10 entries to be processed exactly once*ish*, and say precisely where the "ish" lives.
13. A client sends `HELLO 3`, `SUBSCRIBE ch`, then `GET k`. What happens, and what would have happened under RESP2? Which chapter-7 decision made supporting both cheap?
14. You load a `dump.rdb` produced by a real `redis-server` with default settings and it fails. List the five most likely causes in order, each traceable to one section of ch. 20 or 23.
15. `+@all -@dangerous ~cache:* &news.*` — a client authenticated as this user sends `EVAL "return redis.call('GET', KEYS[1])" 1 other:k`. What happens, where, and why does the answer depend on `COMMAND GETKEYS`?

---

# Appendix F — Bug catalogue

Classics of the genre — each with symptom → cause → the defense this book builds. Your audit findings 1–18 are the first entries; these are the ones still ahead:

| Bug | Symptom | Cause | Defense |
|---|---|---|---|
| args aliasing | garbled keys only under pipelining | decoder buffer reuse across channel | copy at boundary + fuzz + -P 16 test (§28.7) |
| phantom expiry divergence | replica has keys master doesn't | replica expired independently | §9.4 rule; replica branch in expireIfNeeded; digest drill |
| missing DEL propagation | AOF replay resurrects expired/evicted keys | expiry/eviction bypassed propagation hook | one Delete func, hooks inside (§8.3); grep-all-callers audit |
| offset drift | +CONTINUE then protocol garbage | counted non-stream bytes (replies, RDB, handshake) | single feed function owns the counter (§30.4) |
| reply-before-fsync under `always` | "zero loss" config loses acked writes in crash test | reply released before fsync returned | reply-after-flush restructure; crash matrix row (§23.5) |
| snapshot future-leak | RDB contains values written after BGSAVE started | one mutator mutated in place | per-type COW table + isolation test (§23.3) |
| eviction pool corruption | wrong keys evicted; hit rate tanks | full-pool insertion drops best candidate / breaks order (your audit #3!) | property tests: sorted, keyset-parity, top-K (§26.3) |
| WATCH miss on expiry | EXEC succeeds despite watched key vanishing | expiry delete skipped touchWatchedKey | expiry uses full delete path; §33.2 test |
| wake into half-applied batch | BLPOP observes MULTI mid-flight | inline wakeup at signal time | two-phase ready_keys (§31.3); ordering test |
| full-resync loop | replicas never stabilize; master CPU pegged | output-limit kills slow replica → full sync makes it slower | right-size limits + backlog; reproduce-then-fix drill (§30.3) |
| ASK skipped ASKING | keys visible on importer pre-handover | importer accepted un-prefixed queries | refuse + test (§36.3) |
| stale-epoch slot grab | resharding + failover corrupt slot map | believed gossip bitmap without epoch check | epoch gate + UPDATE handling (§36.3) |
| accounting drift | eviction thrashes early / OOM late | a mutation path missed its SizeOf delta | DEBUG RECOUNT-MEMORY invariant after every storm (§26.5) |
| goroutine leak | RSS climbs across client churn | disconnect path missed a registry | pprof-count in done-whens since ch. 7 |
| cron starvation | expiry/rehash never run under load | maintenance behind the request channel with no fairness | ticker case in select + budget per tick (§12.3) |
| script's second propagation path | replica diverges only for keys a script touched | scripting built its own feed instead of reusing the effect stream | one producer, always (§40.3) |
| `XADD *` propagated verbatim | replica's stream IDs differ from master's, forever | auto-generated ID not resolved before propagation | canonicalize in the effect (§42.3) |
| PEL divergence | `XPENDING` differs master vs replica after a group workload | `XREADGROUP` delivery not propagated as `XCLAIM` | propagate group state changes (§42.1) |
| HLL hash mismatch | your HLLs are self-consistent but real Redis counts them wrong | a different hash function than Redis's | port the hash first, test interop before anything else (§44.3) |
| ACL rules applied as a set | a user has permissions their rule string denies | rule list evaluated unordered | ordered left-to-right application + diff `ACL GETUSER` (§47.3) |
| master link hits the ACL gate | replica refuses its own master's writes; looks like a replication bug | `CLIENT_MASTER`/replay flags not exempted from the auth check | exempt at the gate, test with an ACL-enabled master (§47.3) |

Grow this table. Every chaos-found bug earns a row *and* a regression test — the table is the project's scar tissue, and the most interview-valuable artifact in the repo after the README.

---

# Appendix G — Command cookbook

```bash
# ---------- oracle ----------
redis-server --port 6379 --save '' --appendonly no          # clean oracle, no persistence
redis-cli -p 6379 MONITOR                                    # watch what a real client actually sends
diff <(redis-cli -p 6379 LPOS l a RANK -1) <(redis-cli -p 7379 LPOS l a RANK -1)

# ---------- driving dice ----------
redis-cli -p 7379                                            # everything
printf '*1\r\n$4\r\nPING\r\n' | nc localhost 7379            # raw bytes
redis-cli -p 7379 --pipe < bulk.resp                         # mass-load RESP from file
redis-benchmark -p 7379 -q -n 100000 -P 16 -t set,get,incr,lpush,zadd
redis-cli -p 7379 --latency-hist

# ---------- exploring the source ----------
grep -rn "function_name" ~/Code/Learning/redis/src --include='*.c' --include='*.h'
git -C ~/Code/Learning/redis log --oneline -S 'evictionPoolPopulate' -- src/evict.c   # archaeology
ls ~/Code/Learning/redis/tests/unit/ ~/Code/Learning/redis/tests/integration/         # the spec suite

# ---------- persistence forensics ----------
hexdump -C dump.rdb | head -50
redis-check-rdb dump.rdb ; redis-check-aof appendonly.aof    # real Redis's own validators — run on YOUR files (stretch goal)
kill -9 $(pgrep -f 'dice.*7379')                             # the honest crash

# ---------- multi-node locally ----------
for p in 7379 7380 7381; do ./dice --port $p & done
redis-cli -p 7380 REPLICAOF localhost 7379
redis-cli -p 7379 INFO replication
# partition simulation (macOS): use a test-hook flag (--drop-peer host:port) rather than pf; deterministic beats real
# cluster: tests/ch36_cluster.sh up && redis-cli -c -p 7379

# ---------- observing yourself ----------
redis-cli -p 7379 SUBSCRIBE '__keyevent@0__:expired'         # watch active expiry live (ch. 33+)
go tool pprof http://localhost:7380/debug/pprof/goroutine    # if you expose pprof on a debug port — do
scripts/mergelogs.sh node*.log | less                        # ch. 28 cockpit
```

---

# Appendix H — Answer key (selected finals from Appendix E)

**1.** bytes → conn read buffer → decoder buffer → args (copied!) → engine channel → dispatch gates → setGenericCommand → store.Set (hooks: dirty++, watchers, notify) + SetExpire → effect `SET k v PXAT <abs>` → aofBuf + backlog/replica buffers → reply `+OK` to client output buffer → engine tail: aofBuf→write(→fsync per policy), output→socket. Rest points: conn buf, decoder buf, channel, aofBuf, backlog, output buf.

**3.** DEL/UNLINK · SET-family overwrite (dropping old value, TTL) · lazy expiry on lookup · active expiry cycle · eviction · empty-collection deletion after pop/rem · FLUSHDB/ALL · RENAME's target overwrite · MIGRATE's source-DEL · (replica) master's replicated DEL. All must funnel through the store's Delete/overwrite paths where hooks live. The audit-era code's GET-path expiry skipped every hook.

**6.** Granted: replica sent (replid, offset) where replid == master's replid (or replid2 within window) AND offset ≥ backlog start (`master_repl_offset - backlog_histlen`). Wrapped-by-1: offset < start → gates fail → FULLRESYNC. No byte is served from a wrapped region, ever — the check is arithmetic, not luck.

**9.** (a) yes ≤1 s (fsync cadence) · (b) no — reply gated on fsync · (c) yes — async repl, §27.6 · (d) yes — WAIT proves 1 replica *had* it, but if the *promoted* replica is a different one without it, gone; WAIT ≠ chosen-successor durability · (e) yes — minority master's partition-era writes destroyed on demotion full-resync.

**10.** Ranked: output-buffer OOM (any slow consumer, daily at scale) > KEYS-in-prod stall (human error, weekly somewhere) > full-resync loop (undersized limits meet big datasets) > unbounded expiry backlog (mass-TTL events) > split-brain loss (needs partition + failover + writes — rare, worst).

*(The rest are your work — that's the point. Verify against the oracle and the source, not against an answer key.)*

---

# Appendix I — Resource index by chapter

## Set up your library first (30 min, do this today)

```bash
# 1. Redis source — already at ~/Code/Learning/redis  (make -j8 once; you need the binaries)
# 2. Papers directory — the six you implement from, all free PDFs (Appendix D has the links):
#      pugh-skiplists-1990.pdf        siphash-2012.pdf
#      pillai-fs-osdi14.pdf           fsync-failures-atc20.pdf
#      hyperloglog-flajolet07.pdf     swim-dsn02.pdf
#      (Appendix D tier 3 is background; fetch it later or not at all)
# 3. Docs offline: redis.io/docs → the six spec pages (Appendix D tier 1) — save as PDF; you'll read each 2-3×
# 4. Reference implementations to READ (never import):
#    github.com/redis/redis (have it) · github.com/valkey-io/valkey (the fork, actively diverging)
#    github.com/dragonflydb/dragonfly (the §2.4 alternative, C++) · github.com/tidwall/redcon (Go RESP server lib — read AFTER ch. 7 to compare parsers)
```

## Per chapter (beyond the read-alongside boxes)

- **ch. 3**: `ae.c` is the text. Compare with your `async_tcp.go` side by side — write the diff-of-designs in NOTES.md.
- **ch. 15**: after your dict works, read `dictScan`'s comment again; it reads differently once you own a two-table dict.
- **ch. 20**: `tests/integration/aof*.tcl`, `rdb.tcl` — steal test cases wholesale for the crash matrix.
- **ch. 27**: `tests/integration/replication*.tcl` — likewise, especially `replication-psync.tcl`'s parameterized matrix.
- **ch. 34**: the cluster spec page > the code for concepts; the code > the spec for gossip packet details.
- **ch. 38**: real `redis.conf` (repo root) — read every comment once; it is the best-documented config file in open source and half of it is production wisdom in disguise.

**The four papers to read even if you read nothing else**, in this order — each one is code you write:
1. **Pugh 1990** (skip lists) before ch. 17 — you implement it in `ds/skiplist`.
2. **SipHash 2012** before ch. 17 — it decides what your dict hashes with, and why not something faster.
3. **Pillai et al., OSDI 2014** (crash consistency) before ch. 23 — it tells you which of your instincts about files are wrong.
4. **Flajolet et al. 2007** (HyperLogLog) before ch. 44 — you implement it, registers and all.

## Per build chapter — implementation references

| Build chapter | Spec / oracle material |
|---|---|
| **7** server core | `redis.io/topics/protocol` · `networking.c` (ch. 5) · `tests/unit/protocol.tcl` · `github.com/tidwall/redcon` (compare parsers *after* yours works) |
| **11** keyspace + strings | `redis.io/commands/{set,expire,scan,object}` · `tests/unit/{expire,keyspace,scan}.tcl`, `tests/unit/type/string.tcl` · ch. 10 |
| **17** data structures | `dict.c` / `listpack.c` / `t_zset.c` / `intset.c` header comments — the formats *are* the spec · **Pugh 1990**, **SipHash 2012** · ch. 16 |
| **19** collections | `redis.io/commands` per family · `tests/unit/type/{list,hash,set,zset}.tcl` (steal cases wholesale) · ch. 18 |
| **23** persistence | `redis.io/topics/persistence` · `rdb.h` opcodes · **Pillai OSDI 2014**, **Rebello ATC 2020**, **LFS SOSP 1991** · `tests/integration/{aof,rdb,aof-multi-part}.tcl` · ch. 21, ch. 22 |
| **26** eviction | `redis.io/topics/lru-cache` · antirez's LRU-notes post · **Yang OSDI 2020**, **ARC**, **TinyLFU** · `tests/unit/maxmemory.tcl` · ch. 25 |
| **30** replication | `redis.io/topics/replication` · `tests/integration/replication*.tcl` (especially `-psync`) · ch. 29 · **DDIA ch. 5**, then **Raft (paper §5.1–5.4)** and **Dynamo** for contrast |
| **33** tx / blocking / pubsub | `redis.io/topics/{transactions,pubsub,notifications}` · `tests/unit/{multi,pubsub}.tcl` · ch. 32 |
| **36** cluster | `redis.io/topics/cluster-spec` (the assignment) · **SWIM (DSN 2002)** · `tests/unit/cluster/*.tcl` · ch. 35 |
| **40** scripting | `redis.io/docs/latest/develop/programmability/` · `script.c`, `script_lua.c`, `functions.c` · `tests/unit/scripting.tcl`, `tests/unit/functions.tcl` · ch. 39 |
| **42** streams | `redis.io/docs/latest/develop/data-types/streams/` · `t_stream.c`, `rax.c` · `tests/unit/type/stream*.tcl` (three files, steal all of them) · ch. 41 |
| **44** bitmaps/HLL/geo | `bitops.c`, `hyperloglog.c` header comment, `geohash_helper.c` · **Flajolet 2007** · `tests/unit/{bitops,bitfield,hyperloglog,geo}.tcl` · ch. 43 |
| **45** Sentinel | `redis.io/topics/sentinel` · `sentinel.c` · `tests/sentinel/*` · ch. 37 |
| **47** security | `redis.io/docs/latest/operate/oss_and_stack/management/security/` · `acl.c` + `commands/*.json` categories · `tests/unit/acl.tcl` · ch. 46 |
| **48** compatibility | the whole real `tests/` tree · the client libraries' own integration suites · `redis-cli`/`redis-benchmark`/`redis-check-rdb`/`redis-check-aof` |

## The order, condensed (pin above your desk)

```
Part I     1  2
Part II    3  4  5(src)  6  7(BUILD)
Part III   8  9  10(src) 11(BUILD)  12  13
Part IV   14 15  16(src) 17(BUILD)  18(src) 19(BUILD)
Part V    20 21  22(src) 23(BUILD)  24  25(src) 26(BUILD)
Part VI   27 28  29(src) 30(BUILD)
Part VII  31 32(src) 33(BUILD)  34  35(src) 36(BUILD)
Part VIII 37 38
Part IX   39 40(BUILD)  41 42(BUILD)  43 44(BUILD)  45(BUILD)  46 47(BUILD)  48(BUILD)
```

That is the whole navigation system: read them in that order, top to bottom.

---

# Closing

You set out to write a Redis that a client cannot distinguish from Redis. Chapter 48 is the only chapter that can tell you whether you did, because it is the only one where somebody else's tests get a vote — the real TCL suite, four client libraries, and the four command-line tools that ship with the original.

What you will have when it passes: a single binary that speaks RESP2 and RESP3, holds all ten data types with their real encodings and conversion thresholds, expires and evicts by the real algorithms, survives `kill -9` at any point with a documented loss window, writes RDB files real Redis can read and reads the ones it writes, replicates with partial resync, shards across a cluster that fails over on its own, runs Lua and Functions with effects propagation, serves consumer groups, enforces ACLs over TLS — and one honest gap, the C modules ABI, written into your README because a claim of completeness is only worth what its exceptions admit.

Two projects then sit side by side on your disk. ConsulMe taught you what it costs to *refuse* to lose data — quorums, fsync-before-vote, the wall that is Raft. DiceMe taught you what you can buy by *agreeing* to lose a little: a replication stream with no election safety, an eviction that guesses, an expiry that samples, a snapshot that forks. Same networks, same crashes, opposite bets. When "why doesn't Redis just use Raft?" and "why doesn't Consul just use a backlog?" are both questions you answer with trade-offs instead of slogans, you have the thing incidents demand and interviews probe for: judgment about consistency rather than vocabulary about it.

Build the checkpoint. Run the storm. Kill -9 it. Trust nothing you haven't crashed — and nothing a client library hasn't tested.
