# DiceMe — The Complete Book

**Everything needed to build a production-grade Redis from scratch, in Go, starting from the DiceMe you already have.**

Reference source: `~/Code/Learning/redis` @ commit `d22066d09` (version 8.9.241). Every `file:function` reference in this document is verified against that commit. Line numbers drift across versions; function names don't — search for the function, not the line.

> **A note on Redis versions.** The tree you have is a modern one. Some internals were rewritten after Redis 7: the per-db `dict` became `kvstore.c` (one dict per cluster slot), the `expires` dict became `ebuckets.c`/`estore.c` (a bucketed TTL index), and key+value merged into a single `kvobj` allocation. **You should build the classic model** — one hash table for keys, one for expires, exactly the shape your `core/store.go` already has — because it is simpler, it is what every paper and blog post describes, and the new code is an optimization of the same semantics. Where the modern code differs from what you build, this book says so explicitly.

---

## How to use this document

This is not a document you read front to back and then start coding. It is a **book with a reading order**, and the order is:

> concept → the reason the concept exists → how real Redis implements it → what you build → how you know it works

Six parts:

| Part | What it is | When you read it |
|---|---|---|
| **I — Foundations** | The event loop, RESP, memory, and the five core data structures. Everything that isn't Redis-specific but that you cannot build this without. | Before anything. Chapters 1–3 in week one. Chapters 4–5 as reference. |
| **II — Mechanisms** | Expiry, eviction, persistence, replication, Sentinel, Cluster, transactions, threading. The actual systems content. | One chapter per build stage, just before you need it. |
| **III — Redis architecture** | How the real thing is assembled, with end-to-end request traces. | After Part II ch. 9, then re-read after ch. 12. |
| **IV — Reading the source** | 15 guided sessions through the real codebase with exact function anchors. | Each session immediately before the matching build phase. |
| **V — Implementation** | Full specs for 9 phases, plus the production-grade concerns most tutorials skip. | This is where you spend most of your time. |
| **VI — Appendices** | Glossary, command surface, source index, self-test bank, bug catalogue, cookbook. | Lookup, continuously. |

**Rules for using it:**

1. **Never read ahead more than one stage.** Reading about cluster failover while building the skiplist is procrastination that feels like studying.
2. **Build the checkpoint every time one is specified.** The gap between "I read about approximate LRU" and "I watched my eviction pool delete the hottest key because the full-pool branch was backwards" (§Audit, finding 9 — this bug is in your repo right now) is the entire difference between finishing this and abandoning it.
3. **Answer the self-check questions out loud, from memory.** If you can't, you didn't learn it — reread. The questions are in each chapter and collected in Appendix E.
4. **When stuck, the answer is almost always in the real source.** Appendix C maps every concept to its file.
5. **Diff against real Redis constantly.** You have the real thing; `redis-server` on 6379, DiceMe on 7379, same commands to both, compare bytes. This is the single biggest advantage of this project over ConsulMe — the oracle is one process away. Build the diff harness in Phase 0 (§17.4) and never stop using it.

---

## Prerequisites check

You need, before starting:

- **Go**: goroutines, channels, `select`, interfaces, `sync.Mutex`, slices/maps at the "append may reallocate" level, testing. You have this.
- **Networking**: TCP sockets, non-blocking I/O. Your kqueue loop in `server/async_tcp.go` proves you have this.
- **Command line**: `redis-cli`, `redis-benchmark`, `kill -9`, `hexdump -C`, reading logs.

You do **not** need: prior database-internals knowledge, replication theory, or consensus. Redis deliberately avoids consensus almost everywhere — a design position you'll understand by Chapter 12.

---

## The audit — what DiceMe is today

This is a code review of your repo as it stands, written so that every finding becomes a lesson in a specific phase. Nothing here is shaming — several of these bugs are *the classic bugs*, the ones this book exists to teach. The right response to this table is not "fix everything now"; it is "fix each one in its phase, and understand why it was wrong."

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
| `monitoring/` | Prometheus + Grafana scaffolding — wired up in Phase 8 (§28.1). |

That is a legitimate skeleton of Phases 0–1 plus previews of Phases 3–4. You are not starting from zero; you're starting from ~15%.

### The findings

Severity: 🔴 crashes or corrupts, 🟠 wrong behavior a client can observe, 🟡 wrong internals / will block you later.

| # | Where | Finding | Fixed in |
|---|---|---|---|
| 1 | 🔴 `core/resp.go:readSimpleString` (and `readInt64`, `readBulkString`) | Not incremental. `for ; data[pos] != '\r'; pos++` runs off the buffer end on partial input → index-out-of-range **panic**. `readBulkString` slices `data[pos : pos+len]` past the end → panic. TCP is a stream; half a command in one `read()` is normal, not exceptional (§3.3). | Phase 0 |
| 2 | 🔴 `server/sync_tcp.go:readCommands` | One `c.Read` into a 512-byte buffer, then decode. Any command >512 bytes, or a pipelined batch crossing the boundary, is truncated mid-frame → decode error or finding-1 panic → connection dropped. `redis-benchmark -P 10` kills DiceMe today. | Phase 0 |
| 3 | 🔴 `core/evictionpool.go:Push` (full-pool branch) | When the pool is full: compares raw `lastAccessedAt` (ignoring the 24-bit wraparound your own `getIdleTime` handles), **removes `pool[0]` — the best eviction candidate — to make room**, appends without re-sorting (pool order now corrupted), and never deletes the displaced item from `keyset` (leak → those keys can never enter the pool again). Real Redis inserts in ascending-idle order and drops the *smallest*-idle entry (`evict.c:evictionPoolPopulate`). | Phase 4 |
| 4 | 🔴 `core/aof.go:DumpAllAOF` | Opens with `O_APPEND` — every BGREWRITEAOF appends the **entire dataset again** (check `dice-master.aof`: duplicates already). No temp-file + rename (§29.2), no fsync, no TTL preservation, values containing spaces corrupt the file (`strings.Split(cmd, " ")`), and there is no load-on-boot at all — persistence is write-only. Also runs synchronously in the event loop despite the BG name. | Phase 3 |
| 5 | 🟠 `core/eval.go:EvalAndRespond` default branch | Unknown command falls through to `evalPING` → `FOOBAR` returns `+PONG`. Must be `-ERR unknown command 'foobar'`. Silent-wrong is the worst failure mode a server can have. | Phase 0 |
| 6 | 🟠 `server/async_tcp.go` main loop | `syscall.Kevent(kqueueFD, nil, events, nil)` — nil timespec blocks **forever**. The expiry cron runs only when client traffic happens to wake the loop; on an idle server, keys never actively expire. Real `ae.c` computes the poll timeout from the nearest timer (§2.5). | Phase 0 |
| 7 | 🟠 `core/eval.go:evalSET` / `evalEXPIRE` | `EX 0` silently ignored (`exDurationMs > 0` guard) — real Redis errors `invalid expire time`. `EXPIRE` with 0/negative TTL must **delete** the key and report 1, not set a past timestamp that leaves a phantom key (§7.2). `TTL` rounds down (real: rounds to nearest — 900 ms left is `1`, not `0`). | Phase 1 |
| 8 | 🟠 `core/store.go:Put` | Evicts when `len(store) >= config.KeysLimit` — a **key count** of 100, not a memory budget. Your own storm generator trips constant eviction. Becomes `maxmemory` byte accounting in Phase 4 (§23.2). | Phase 4 |
| 9 | 🟠 `server/async_tcp.go:WaitForSignal` | `for atomic.LoadInt32(&eStatus) == BUSY {}` — busy-spins a full core while waiting. Also `switch eStatus` later reads the variable non-atomically. Dies with the architecture change in Phase 0 (§14.4). | Phase 0 |
| 10 | 🟡 `core/eval.go:evalINCR` | Parse error ignored (`i, _ := strconv.ParseInt`), no int64 overflow check (real Redis: `increment or decrement would overflow`), and error text doesn't match Redis's `ERR value is not an integer or out of range` — your diff harness will flag every mismatch like this. | Phase 1 |
| 11 | 🟡 `core/expire.go:DeleteExpiredKeys` | The 25%-loop has no **time budget**. After mass expiry (imagine 1M keys with the same deadline), the loop runs unbounded inside the cron and blocks all clients. Real Redis caps the cycle at a time slice (`expire.c:activeExpireCycle`, §7.3). | Phase 1 |
| 12 | 🟡 `core/eviction.go` | `evictAllkeysLRU` evicts 40% of KeysLimit in one burst (real Redis frees just enough to get under `maxmemory`); pool is populated with only 5 samples once per burst; `volatile-*` policies (sample from `expires`, not `store`) don't exist yet. | Phase 4 |
| 13 | 🟡 `core/store.go:Rename` | Calls `Put(newKey)`, which can trigger eviction mid-rename — with terrible luck the evictee is the very object being renamed. Rename must be a plain swap, never a capacity-checked insert. Also: first-ever `Del` before any `Put` panics on the nil `KeyspaceStat[0]` map. | Phase 1 |
| 14 | 🟡 `core/eval.go:evalKEYS` | Uses `path.Match`: `/` is special, `[` unclosed returns an error (your `ERR invalid pattern`), neither matches Redis's `stringmatchlen` glob (which treats malformed patterns literally and errors on nothing). Write your own matcher in Phase 1 — it's 40 lines. | Phase 1 |
| 15 | 🟡 `main.go` | `wg.Add(2)` but `RunAsyncTCPServer`'s goroutine never calls `Done`; the process only exits because the signal handler calls `os.Exit`. Clean shutdown (finish in-flight command → flush AOF → close listeners) is a Phase 0 requirement. | Phase 0 |
| 16 | 🟡 `respond()` → `core/comm.go` | Replies written straight to the fd with a single `syscall.Write` — partial writes silently drop bytes, and there is no output buffer, so one slow client can block the loop. No `TCP_NODELAY` on accepted fds either (raw-syscall path — Nagle adds ~40 ms to small replies). | Phase 0 |
| 17 | 🟡 `core/resp.go:readLength` | Can't represent `-1`, so `$-1\r\n` (null bulk) mis-parses as an empty string with a wrong delta — matters the moment you *decode* replies (replication, Phase 5, speaks RESP back at you). | Phase 0 |
| 18 | 🟡 `core/eval.go:evalGET` | Dead second `hasExpired` check — `Get` already deleted the key. Harmless; symptomatic of expiry logic living in two places. One choke point: `expireIfNeeded` (§7.2). | Phase 1 |

Two things in the repo are **more right than they look**: the two-nibble `TypeEncoding` scheme is exactly `robj`'s `type`/`encoding` split and survives to the end of the project, and `getIdleTime`'s wraparound handling matches `evict.c:estimateObjectIdleTime`. Keep both.

---

## The map

```
STAGE  1  Server core + RESP + command table    1 wk    Part I ch.2-3, ch.14   → Phase 0   ← rebuild on solid ground
STAGE  2  Object model, strings, expiry         1.5 wk  Part II ch.6-7         → Phase 1   ← first milestone
STAGE  3  Core data structures as libraries     2 wk    Part I ch.5            → dict, listpack, skiplist, intset (standalone + tests)
STAGE  4  Collections: list/hash/set/zset       2 wk    Part IV s.12           → Phase 2   ← genuinely useful
STAGE  5  Persistence: RDB + AOF                2.5 wk  Part II ch.9           → Phase 3   ← first hard problem (no fork in Go)
STAGE  6  Eviction + maxmemory                  1 wk    Part II ch.8           → Phase 4
STAGE  7  Replication                           3 wk    Part II ch.10          → Phase 5   ← the wall
STAGE  8  Transactions, blocking, pub/sub       1.5 wk  Part II ch.13          → Phase 6
STAGE  9  Cluster                               3 wk    Part II ch.12          → Phase 7   ← the second wall
STAGE 10  Production hardening                  2 wk    Part V ch.28-30        → Phase 8
```

## Time budget

Hours are **focused working hours**, not elapsed time. At ~25 h/week the total lands near **4.5 months**; at ~10 h/week, closer to **11**. The ratios matter more than the absolutes — if replication takes 3× the estimate, that's normal; if Phase 0 does, something is wrong with your setup and you should ask.

| Part / Chapter | Read | Build | Total | Notes |
|---|---|---|---|---|
| **Front matter + ch. 0** | 1 h | — | **1 h** | do this first |
| **PART I — FOUNDATIONS** | | | **26 h** | |
| 1 What Redis is | 2 h | — | 2 h | re-read after ch. 14 |
| 2 The event loop | 3 h | 2 h | 5 h | build: benchmark your kqueue loop vs goroutine-per-conn |
| 3 RESP | 2 h | 2 h | 4 h | build: fuzz your existing parser, watch finding-1 fire |
| 4 Memory | 3 h | — | 3 h | the fork/COW problem — read before Phase 3 |
| 5 Data structures | 4 h | **8 h** | 12 h | sketches only; real builds in Stage 3 |
| **PART II — MECHANISMS** | | | **26 h** | |
| 6 Object model + keyspace | 2 h | — | 2 h | |
| 7 Expiration | 2 h | — | 2 h | §7.4 (expiry × replication) is the highest-value section in the book |
| 8 Eviction | 2 h | — | 2 h | |
| 9 Persistence | **4 h** | — | 4 h | read twice; the crash matrix in §9.6 |
| 10 Replication | **5 h** | — | 5 h | read before Stage 7, re-read halfway through it |
| 11 Sentinel | 2 h | — | 2 h | |
| 12 Cluster | **4 h** | — | 4 h | |
| 13 Tx, blocking, pub/sub | 3 h | — | 3 h | |
| 14 Threading model | 2 h | — | 2 h | §14.4 decides your whole architecture — read in week one |
| **PART III — ARCHITECTURE** | 4 h | — | **4 h** | re-read after Stage 7 |
| **PART IV — SOURCE READING** | | | **30 h** | spread across stages, never in one block |
| **PART V — IMPLEMENTATION** | | | **≈370 h** | |
| 17 Setup + diff harness | 1 h | 4 h | 5 h | |
| 18 Phase 0 server core | — | **25 h** | 25 h | |
| 19 Phase 1 strings + expiry | — | **35 h** | 35 h | |
| 20 Stage-3 data-structure libraries | — | **45 h** | 45 h | dict 10 h · listpack 15 h · skiplist 10 h · intset 4 h · glue 6 h |
| 21 Phase 2 collections | — | **40 h** | 40 h | |
| 22 Phase 3 persistence | — | **45 h** | 45 h | RDB 20 h · AOF 15 h · rewrite 10 h |
| 23 Phase 4 eviction | — | **20 h** | 20 h | |
| 24 Phase 5 replication | — | **55 h** | 55 h | the wall |
| 25 Phase 6 tx/blocking/pubsub | — | **25 h** | 25 h | |
| 26 Phase 7 cluster | — | **55 h** | 55 h | the second wall |
| 27 Phase 8 stretch | — | optional | — | pick à la carte |
| 28 Production concerns | 2 h | **15 h** | 17 h | metrics, limits, chaos — wires up your `monitoring/` dir |
| 29 Crash safety | 2 h | included in Phase 3 | 2 h | read before Phase 3 |
| 30 Debugging | 2 h | — | 2 h | read before Stage 7; re-read when stuck |
| **PART VI — APPENDICES** | ref | — | — | lookup only |
| | | | | |
| **TOTAL** | ≈80 h | ≈375 h | **≈455 h** | ≈4.5 months at 25 h/wk, ≈11 months at 10 h/wk |

**Where the time actually goes:**

- **Replication + Cluster are 110 h** — a quarter of the project, and the two subsystems almost every "build Redis" tutorial skips entirely. They are also what interviewers actually ask about.
- **Persistence is 45 h** and *harder in Go than in C*, because you cannot `fork()`. Snapshot-while-serving without copy-on-write is the most original engineering problem in this project (§9.5).
- The data-structure libraries (45 h) are the computer-science core; everything after Stage 4 is systems engineering.
- Reading is under a fifth of total time — and a third of *that* is reading the real Redis source, not prose. This is a building project, not a studying project.

## Checkpoint demos

Milestones you can actually show someone. Useful for morale, and for interviews.

| After | Demo |
|---|---|
| Stage 1 | `redis-cli -p 7379` works against DiceMe: pipelined, 1000 concurrent clients, `redis-benchmark -P 16` runs clean, `FOOBAR` gets a proper error |
| Stage 2 | `SET k v EX 2`, watch `TTL` count down, key vanishes on an **idle** server; `OBJECT ENCODING` shows `int`/`embstr`/`raw` correctly |
| Stage 4 | `ZADD`/`ZRANGEBYSCORE` on 1M members; `OBJECT ENCODING` flips `listpack` → `skiplist` at exactly the configured threshold |
| Stage 5 | `kill -9` mid-benchmark, restart, nothing acknowledged is lost (AOF always) or ≤1 s is lost (everysec); RDB file loads in real `redis-server` *(stretch)* |
| Stage 6 | fill past maxmemory; approximate LRU evicts cold keys while a hot-key workload stays resident; plot the eviction-age histogram |
| Stage 7 | writes on master appear on replica in <10 ms; cut the link mid-stream, reconnect, **partial** resync (no full RDB transfer — watch the logs prove it) |
| Stage 8 | two clients race `WATCH`/`MULTI`/`EXEC`, exactly one wins; `BLPOP` wakes the instant another client pushes |
| Stage 9 | 6-node cluster (3 masters + 3 replicas), `kill -9` a master, automatic failover, `redis-cli -c` follows redirects transparently |

**If you only have 150 hours**, the minimum path to genuinely understanding Redis is: ch. 1–5 (26 h) → Phase 0 (25 h) → Phase 1 (35 h) → dict + skiplist only (20 h) → Phase 3 (45 h)… which already exceeds it. So the honest 150-hour version keeps Phases 0–3 with only strings + hashes + zsets, and replaces the replication/cluster *builds* with the *reading* sessions (Part IV s.11, s.14) plus operating the real thing. You lose the two deepest lessons but keep a durable, correct, single-node Redis. Your call — just make it deliberately, not by drifting.

---

## Chapter 0 — How to study this material

You've built one round of DiceMe already, so you know the loop: read a little, build, get confused, read the source. These techniques sharpen that loop.

### Reading C source when you write Go

The Redis codebase is famously readable C, but three idioms trip up Go programmers:

1. **Intrusive data structures and `void*`.** A `listNode` holds `void *value`; a dict entry holds untyped key and value pointers. In Go you'll use ordinary typed structs — the *algorithm* is what you're extracting, not the memory layout.
2. **Flags packed into ints.** `CLIENT_MULTI | CLIENT_DIRTY_CAS` — grep the `#define` blocks at the top of `server.h` whenever you meet a constant. Keep `server.h` open in a tab permanently; it is the schema of the entire program: `struct client`, `struct redisDb`, `struct redisServer` are the three types everything else orbits.
3. **Error handling by return code + `goto err`.** Translate mentally to `if err != nil` and move on; don't study the cleanup ladders.

### The three-pass source session

Every Part IV session, same method:

1. **Skim (15 min).** Function signatures and comments only, in the listed files. Goal: what lives here.
2. **Trace (1 h).** Follow one concrete command through the one entry point the session names, writing the call chain by hand: `readQueryFromClient → processInputBuffer → processCommand → call → setCommand → setGenericCommand`.
3. **Reconstruct (30 min).** Close the source. Write the mechanism from memory as pseudocode. The gaps are your reading list for pass two.

### Note-taking that works here

Keep one `NOTES.md` per stage in the DiceMe repo. Three sections:

- **Mechanism** — how it works, your own words, no quoting.
- **Why** — the failure it prevents or the benchmark it wins. If you can't write this, you memorized rather than understood.
- **Open questions** — literally a list. Revisit at each stage's end; you'll answer most yourself.

### When you're stuck

In order — do not skip to the bottom:

1. **Re-read the self-check questions** for that chapter. They're diagnostic: the one you can't answer names your gap.
2. **Ask the oracle.** Run the same command sequence against real Redis and diff output byte-for-byte (§17.4). Half your bugs are semantics you guessed instead of checked.
3. **Read the real Redis test** for that behavior — `tests/unit/` and `tests/integration/` in the redis repo. `tests/unit/expire.tcl` is a specification of expiry disguised as a test file; `tests/integration/replication-psync.tcl` is the same for partial resync.
4. **Read the real implementation** at the anchor in Appendix C.
5. **Add logging and run it.** Replication and cluster bugs are almost never reasoned out from source; they're observed. See ch. 30.
6. **Ask.** Come with: what you expected, what happened, what you already tried.

### Pacing rules

- **One stage at a time.**
- **Never more than 3 days without running code.**
- **Timebox rabbit holes to 2 hours.** Write the question in `NOTES.md`, move on, come back.
- **Take the checkpoints.** After Phase 3 you have a durable, genuinely useful cache. Tag it `v0.1`, update the README, show someone. Momentum is a resource and Stage 7 will consume a lot of it.

### What this book deliberately does *not* cover

Being explicit so you know the edges:

- **Modules API** (`module.c`) — a plugin ABI, not database internals.
- **Query engine / vector search / JSON / TimeSeries** — modules, separate products.
- **TLS** (`tls.c`) — transport plumbing; bolt on `crypto/tls` at the very end if you want; it teaches Go, not Redis.
- **ACLs in depth** (`acl.c`) — sketched in Phase 8 only.
- **The 8.x internals as a build target** — `kvstore`, `ebuckets`, `kvobj`, `client_comp.c`, `defrag.c`. You read them for contrast; you build the classic model.
- **io_uring and platform-exotic I/O** — ch. 2 covers kqueue/epoll conceptually; you build on Go's netpoller.
- **Client-side caching (tracking)** — mentioned in Phase 8; a weekend feature after everything else works.

---
---

# PART I — FOUNDATIONS

---

# Chapter 1 — What Redis actually is

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "Introduction to Redis" (`redis.io/docs`), then the command pages `redis.io/commands/{set,expire,lpush,zadd}` — read the **complexity** line on each | 45 min |
> | **[CORE]** | "Latency Numbers Every Programmer Should Know" — `gist.github.com/jboner/2841832` | 10 min |
> | **[OPT]** | The `MANIFESTO` file at the redis repo root — antirez's design values; 10 minutes that explain fifty decisions | 10 min |
> | **[OPT]** | antirez, "Redis persistence demystified" — `antirez.com` (2012, mental model unchanged) | 30 min |

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

- **A GET is ~100 ns of actual work** (one hash lookup) wrapped in 50–500 µs of network round trip. The server is never the bottleneck for one client; the network is. Therefore **pipelining** (§3.6) matters more than server micro-optimization, and one core really does serve 100k+ ops/sec.
- **fsync costs ~1 ms** — 10,000× the operation it protects. Persistence must be decoupled from the command path: AOF buffers in memory and fsyncs on a policy (§9.3), never inline.
- **Memory is the budget.** At ~100 bytes of overhead per key, 100M keys is 10 GB before any data. Hence the obsessive small-object encodings (§5.4, §6.2): a hash of 50 short fields is one packed byte array, not 50 heap objects.
- **One core, no locks** means every command is atomic by construction. `INCR` is atomic not through clever locking but because nothing else can possibly run concurrently. This is the deepest simplification in the design — and in Go you will have to *choose* it deliberately rather than inheriting it from C's single thread (§14.4). Your current code already lives this way by accident: the kqueue loop executes commands inline, one at a time.

## 1.2 A data-structure server, not a cache

The correct one-line description: **a server that exposes data structures over a wire protocol.** `LPUSH` is `list.insert(0, x)`; `ZRANGEBYSCORE` is a range query on an ordered map; `HSET` is `map[field] = value` inside `map[key]`.

This framing explains:

- **Commands are the API and there is no query planner.** Each command's complexity is documented and the user is the planner. `KEYS *` is O(N) and blocks the server for its whole runtime — and that's *your* fault, says Redis; the docs told you. (`SCAN` is the apology — §5.3.)
- **Atomicity is per-command**, plus MULTI/EXEC batches. One thread, one command at a time; each command sees a consistent world and leaves one.
- **Five-plus physical representations per logical type.** The contract is the structure's *behavior*, so the server freely swaps representation as data grows (§6.2). A 5-field hash is a flat byte array; a 5M-field hash is a real hash table. Same commands, same semantics, 10× memory difference.

## 1.3 What "production-grade" means for this project

Real Redis is production-grade because of what happens at the edges, not the center. A GET/SET server is a weekend — you've already had that weekend. These are the edges, and they are the actual syllabus:

| Edge | Chapter |
|---|---|
| A key expires *during* a read — who deletes it, and what may a **replica** do differently? | 7 |
| Memory is full — which key dies, and how do you choose without scanning everything? | 8 |
| `kill -9` mid-write — what's on disk, what loads at boot, exactly how much is lost? | 9 |
| The replica's TCP link drops for 3 seconds — full resync or continue from an offset? | 10 |
| The master dies — who notices, who decides, who promotes, who un-promotes the old master when it returns? | 11–12 |
| A client WATCHes a key that expires before EXEC — does the transaction run? | 13 |
| A client blocks on BLPOP and another pushes — exactly when, in the event loop, does it wake? | 13 |
| A slow client can't drain replies as fast as you produce them — whose memory fills, and what pops first? | 28 |

Every one of these has a precise, tested answer in the real source. Building to those answers — not to "seems to work" — is the project.

## 1.4 When Redis blocks, and why that's the cardinal sin

One thread means one crime: **anything O(N) on the command path stalls every client.** The codebase is shaped by avoiding it:

- Deleting a 10M-entry hash would block for seconds → **lazy free**: unlink the key now, hand the object to a background thread (`lazyfree.c`, §14.2).
- Rehashing a 100M-key dict at once would block → **incremental rehash**: a few buckets per operation (§5.2).
- Expiring a million dead keys at once would block → **sampling with a time budget** (§7.3) — the budget your `DeleteExpiredKeys` is missing (audit #11).
- Saving 10 GB inline would block for minutes → **fork + copy-on-write**: the child sees a frozen snapshot for free (§9.2). The one trick Go cannot copy; §9.5 is what to do instead.
- `KEYS` blocks → **SCAN**: a cursor iterator doing O(1) work per call that still guarantees every key present throughout the scan is returned at least once, *even across rehashes* (§5.3 — the reverse-binary cursor, the cleverest small algorithm in Redis).

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

# Chapter 2 — The event loop

> ### 📚 Read alongside — 3 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `ae.c` + `ae.h` — the whole thing; the cleanest event loop you'll ever read. Entry: `aeMain`, `aeProcessEvents`. | 1 h |
> | **[CORE]** | `ae_kqueue.c` — you wrote one of these (`server/async_tcp.go`); compare choices line by line | 20 min |
> | **[USE]** | "The C10K problem", Dan Kegel — `kegel.com/c10k.html`; the document that named the design space | 30 min |
> | **[OPT]** | Any good "how the Go netpoller works" writeup — you build on one of these whether you like it or not | 45 min |

## 2.1 The problem the event loop solves

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

## 2.2 Why Redis stays single-threaded anyway

Even with the loop, Redis could hand command execution to worker threads. It doesn't, because:

- Work per command is ~1 µs; lock overhead plus cache-line bouncing would eat a large fraction of it.
- Atomicity comes free — no lock ordering, no deadlocks, no torn reads, ever.
- The bottleneck is network and memory bandwidth, not CPU. When a core saturates, the official answer is more instances (Cluster, ch. 12).

Where Redis *does* use threads — the complete list — is ch. 14: background fsync/close/free (`bio.c`), lazy freeing, and optional I/O threads that only read/parse input and write output, never execute.

## 2.3 The Go decision: you already have an event loop

Your raw kqueue loop works, and writing it taught you the reactor pattern — mission accomplished, keep the file as a trophy. But carrying it forward fights the platform:

- Go's runtime **already multiplexes goroutines onto epoll/kqueue** (the netpoller). `conn.Read` parks a ~4 KB goroutine; the netpoller wakes it. Same syscalls you make by hand, plus a scheduler you don't have to write.
- Raw syscalls forfeit the stdlib (`bufio`, `crypto/tls`, portability to Linux, where your `syscall.Kevent` code won't even compile) and win nothing measurable at any scale you'll test.

So Phase 0 makes the architecture explicit. This book's recommendation:

> **Goroutine-per-connection for I/O; one owner goroutine for the store.** Each connection goroutine reads bytes, runs the incremental RESP parser, and sends complete commands into one channel. A single **engine goroutine** receives commands, executes them against the store, and hands reply bytes back. The engine goroutine *is* Redis's main thread: per-command atomicity, zero locks on the data path, and `WATCH`/`MULTI`/blocking/eviction semantics all stay exactly as simple as they are in C.

§14.4 works the alternatives (global mutex, sharded locks, lock-per-key) and shows how each quietly breaks `MULTI`, `RANDOMKEY`, `FLUSHALL`, or eviction accounting. The channel *is* your event queue; `select` over {command channel, cron ticker, shutdown} *is* your `aeProcessEvents`.

## 2.4 Socket details that will bite you

- **`TCP_NODELAY`** — Nagle's algorithm delays small writes ~40 ms hoping to coalesce. `anet.c:anetEnableTcpNoDelay` disables it on every accepted connection; Go's `net.TCPConn` disables it by default (one point to the stdlib — your raw-syscall path never did this, audit #16).
- **maxclients** — track connections against a limit (Redis default 10000); over it, reply `-ERR max number of clients reached` and close. Never silently drop.
- **Read buffer discipline** — a client may legally send a 512 MB bulk string. Grow the buffer as bytes arrive; enforce `proto-max-bulk-len` by erroring, never by pre-allocating.
- **Partial writes** — `write()` on a non-blocking socket may take 3 of your 10 KB. Loop, or let `bufio`/the writer goroutine handle it. Your `respond()` currently fires one `syscall.Write` and hopes (audit #16).
- **Output buffering** — replies queue per client; the loop drains them when the socket allows. Enforce **client-output-buffer-limits** or one slow subscriber OOMs you (§28.4). This is the most common way real Redis servers die in production.

## 2.5 The iteration, precisely

Memorize this ordering; three later chapters hang off it (blocking clients §13.3, AOF flush §9.3, replica feeding §10.5):

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

## Self-check — Chapter 2

1. What does `kqueue`/`epoll_wait` actually return, and why is the loop O(ready) rather than O(clients)?
2. Why must the poll timeout equal the nearest timer deadline? Name the bug in your own repo caused by ignoring this.
3. Where in the iteration does a command execute? Where do its reply bytes touch the socket?
4. Why is goroutine-per-connection not a betrayal of the event-loop lesson in Go?
5. Name two features that quietly break under a lock-per-key store design.
6. A client sends one 512 MB `SET`. Walk your read path: what grows, what's checked, what must never be pre-allocated.

---

# Chapter 3 — RESP: the wire protocol

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "RESP protocol spec" — `redis.io/topics/protocol`; RESP2 carefully, RESP3 skim | 1 h |
> | **[USE]** | `networking.c`: `processInputBuffer`, `processMultibulkBuffer`, `processInlineBuffer` — the shape of a real incremental parser | 45 min |
> | **[OPT]** | Your `core/resp.go` — reread after the spec; list what's missing before checking against §3.3 | 15 min |

## 3.1 The shape of RESP2

First byte declares the type; `\r\n` terminates every line. Five types:

```
+OK\r\n                                       simple string (no CRLF inside)
-ERR unknown command 'FOO'\r\n                error (first word = error class)
:1000\r\n                                     integer (may be negative)
$5\r\nhello\r\n                               bulk string ($-1\r\n = null)
*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n     array (*-1\r\n = null array, *0\r\n = empty)
```

Requests are **always arrays of bulk strings** (or inline, §3.4). Replies use all five. Bulk strings are binary-safe — the length prefix means payloads may contain `\r\n`, zero bytes, JPEGs. This is why parsing by scanning for delimiters is wrong for bulk data, and why `readLength` needs to handle `-1` (audit #17).

## 3.2 The rules that make it good

Protocol-design lessons worth keeping:

- **Length-prefix data, delimit control.** Parsing never scans payload bytes: read the `$N` header, then exactly N bytes, then 2. O(1) per element after the header.
- **Human-typeable.** `telnet` in, type `PING`, get `+PONG`. Inline commands exist purely for this.
- **Type-tagged replies** — clients decode without out-of-band schema.
- **Errors are strings with a convention.** The first word (`ERR`, `WRONGTYPE`, `MOVED`, `ASK`, `NOAUTH`, `LOADING`, `BUSYKEY`…) is machine-parseable; the rest human. **Copy Redis's error texts byte-for-byte** — client libraries pattern-match them, and your diff harness (§17.4) will flag every deviation. `-WRONGTYPE Operation against a key holding the wrong kind of value` — exactly that.

## 3.3 The parser must be incremental — the audit-#1 fix

TCP is a stream. One `read()` may deliver half a command, exactly one, or three and a half (pipelining). The parser is therefore a **resumable state machine over an accumulating per-connection buffer**:

- Try to parse one complete command from the buffer.
- If the bytes run out mid-frame: return "incomplete" — a *normal* result, not an error — consuming nothing, and re-enter when more bytes arrive.
- If a full command parses: consume exactly its bytes, hand the command off, and immediately try again (pipelining).

`processMultibulkBuffer` keeps the resume state on the client struct: `multibulklen` (elements still expected) and `bulklen` (bytes still expected for the current element). The parse continues across event-loop iterations without re-scanning.

Your Phase 0 fix list for `core/resp.go`:

1. Every `readX` gets a "not enough data" return path; no indexing past `len(data)`, ever. Fuzz it (§18.2).
2. `readLength` handles `-1` (null bulk/array) and rejects garbage.
3. Enforce limits mid-parse: element count (>1M → `-ERR Protocol error: invalid multibulk length`), bulk length (> `proto-max-bulk-len`, default 512 MB), total buffer growth.
4. On protocol error: send the error, then **close the connection**. A desynced stream has no findable next-command boundary.

## 3.4 Inline commands

If the first byte isn't `*`, treat the line as a space-separated command: `PING\r\n`, `SET k v\r\n`. Twenty lines (`processInlineBuffer` adds quote handling; optional), and `nc`/`telnet` debugging stays pleasant forever. Note your AOF currently *depends* on inline-style splitting — that dependency dies in Phase 3 when the AOF becomes proper RESP arrays with binary-safe values (audit #4).

## 3.5 Replies and the output buffer

Commands never write to sockets. They append to the client's **output buffer** (`addReply*` family, networking.c); the loop drains buffers when sockets allow — mostly an eager write in `beforeSleep`, falling back to a writability event only when the socket backs up. Consequences:

- A command's latency never includes a slow client's socket.
- Output can accumulate → output buffer limits (§28.4).
- Per-client ordering is trivial.

In Go: a per-connection writer (goroutine draining a channel, or a mutex-guarded `bufio.Writer` flushed after each batch). The engine never blocks on any client's socket; a client over its buffer limit gets killed, same as Redis.

## 3.6 Pipelining

A client sends N commands without awaiting replies; the server executes in order, replies in order. Nothing special is needed *if* your parser loops (§3.3) and your writer preserves order. The win is pure latency arithmetic: 10,000 sequential SETs at 0.5 ms RTT = 5 s; batches of 100 = 50 ms. `redis-benchmark -P 16` is your standard load from Phase 0 onward — today it kills DiceMe inside a second (audit #2). Make "survives `-P 16` for 60 s" a Phase 0 done-when.

## 3.7 RESP3 in one box (build in Phase 8, know about now)

`HELLO 3` upgrades the connection. Adds: typed maps (`%`), sets (`~`), doubles (`,`), booleans (`#`), big numbers (`(`), verbatim strings (`=`), one unified null (`_`), and **push frames** (`>`) — server-initiated messages (pub/sub, invalidation) that interleave with replies. RESP2 pub/sub fakes push with arrays; RESP3 makes it a frame type. Design your reply API now so this slots in later: commands should emit *logical* replies ("map", "double", "ok") and a per-connection serializer renders RESP2 or RESP3. If your eval functions return raw `[]byte` forever (as `core/eval.go` does today), RESP3 becomes a rewrite instead of a serializer.

## Self-check — Chapter 3

1. Why length-prefix bulk strings instead of escaping? Two properties gained.
2. The buffer holds `*2\r\n$4\r\nLLEN\r\n$6\r\nmyl` — what exactly does your parser do, and what does it absolutely not do?
3. Why must a protocol error close the connection rather than resync to the next `*`?
4. Where do reply bytes live between execution and the socket write, and which two failure modes does that placement create?
5. What two server properties make pipelining "free"?
6. What do RESP3 push frames fix that RESP2 pub/sub fakes?

---

# Chapter 4 — Memory

> ### 📚 Read alongside — 3 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "Memory optimization" — `redis.io/topics/memory-optimization`; the encodings/thresholds section | 30 min |
> | **[CORE]** | `sds.h` header comment + the five `sdshdr` struct variants — how far Redis goes to save 3 bytes per string | 30 min |
> | **[USE]** | `zmalloc.c`: `zmalloc_used_memory` — how used-memory accounting actually works (a counter, not a query) | 20 min |
> | **[OPT]** | antirez, "Redis latency spikes and the Linux kernel" + any fork/COW writeup — background for §9.2 | 40 min |

## 4.1 Why memory layout is the whole game

In a disk database, memory layout barely matters — disk dominates. In Redis, memory *is* the storage medium, so layout determines both capacity (how many keys fit) and speed (cache locality). Two numbers to hold:

- **A pointer costs 8 bytes, and a heap allocation costs ~16–48 bytes of overhead** (allocator header + size-class rounding). A linked list of 100 3-byte strings costs ~100 × (8 next + 8 prev + 8 value ptr + 48 alloc overhead + data) ≈ 7 KB for 300 bytes of payload. This single observation motivates listpack (§5.4), intset, SDS, and embstr.
- **A cache miss costs ~100 ns; a cache hit ~1 ns.** Chasing 5 pointers = 5 potential misses. A flat byte array you scan linearly prefetches beautifully. For small N, **O(N) linear scan of packed bytes beats O(1) pointer-chasing** — that's why small hashes/sets/zsets use packed encodings and convert only past a threshold (§6.2).

## 4.2 How C Redis counts every byte

`zmalloc` wraps `malloc` and keeps a global `used_memory` counter, incremented by the *allocator-rounded* size of every allocation. `maxmemory` enforcement (§8.1) compares this counter — an O(1) read — against the limit on every write command. `INFO memory`'s `used_memory` is this counter; `used_memory_rss` is what the OS reports; the ratio is **fragmentation** (allocated-but-unused slack inside the allocator). Redis ships an active defragmenter (`defrag.c`) — out of scope, but know why it exists.

## 4.3 The Go problem #1: you can't count bytes

Go gives you no per-allocation hook. Your options for `maxmemory` accounting, in order of usefulness:

1. **Logical accounting (build this).** Every stored object reports its approximate size via a `SizeOf()` you write: string = len + header constant; listpack = len(buf); dict = buckets × entry size + per-key/value sizes. Maintain a running counter updated on every Put/Del/mutation — the zmalloc pattern, one level up. Not exact; *consistently* inexact, which is all eviction needs.
2. `runtime.ReadMemStats` — stop-the-world-ish, coarse (includes GC slack, goroutine stacks, your replication buffers), and laggy. Use only as an `INFO` cross-check.
3. Cgo + manual allocation — defeats the purpose of the exercise.

Design consequence: **every value type you add in Phase 2 implements `SizeOf()` from day one.** Retrofitting accounting onto five data types is a week; carrying it along is free.

## 4.4 The Go problem #2: no fork, no copy-on-write

The most consequential C-vs-Go difference in this entire project.

C Redis snapshots (RDB, §9.2) by calling `fork()`. The child gets a **page-table copy** of the whole address space — a frozen, point-in-time view, at nearly zero upfront cost. Parent keeps serving writes; the kernel copies pages only when the parent modifies them (**copy-on-write**). The child serializes its frozen view at leisure.

Go cannot `fork()` — `syscall.ForkExec` only supports fork+exec (a fresh program), because a bare fork duplicates a process whose runtime expects its scheduler and GC threads to exist, and they don't survive the fork. So you must produce a point-in-time snapshot while serving writes, by construction rather than by kernel magic. Your realistic options, all covered in §22.3:

| Strategy | Cost | Fidelity |
|---|---|---|
| **Stop-the-world serialize** | pause = full serialize time (seconds per GB) | perfect |
| **Copy keyspace, serialize copy** | pause = O(keys) map copy (~100 ms per M keys), then 2× memory for shared values | perfect, if values are copy-on-write-by-discipline |
| **Copy-on-write by hand** (versioned store or shadow dict of pending changes) | complexity | perfect |
| **Fuzzy iteration** (serialize live under cursor, like SCAN) | cheap | *not* point-in-time — inconsistent snapshot; wrong for RDB semantics |

The recommended build: **copy-the-index + immutable-values discipline** — snapshot = shallow copy of the `map[string]*Obj` plus the rule that *mutations never modify a value in place; they replace the `*Obj`* (strings already work this way; collections need care — §22.3 handles each type). Then a background goroutine serializes the shallow copy while the engine keeps replacing pointers in the live map. This is persistent-data-structures thinking, and it is the single most instructive Go-specific design problem in the book.

## 4.5 GC pressure, briefly

Go's GC scans pointers. 100M `map[string]*Obj` entries = long mark phases and latency spikes. Real mitigations, in the order you should reach for them: fewer, bigger allocations (listpack does this for free — packed encodings are GC-friendly too); avoid pointers in hot structs (store small strings by value where possible); `GOGC`/`GOMEMLIMIT` tuning last. Don't optimize this preemptively — measure with `redis-benchmark` + `runtime/metrics` in Phase 8. But know *why* the packed encodings you build in Stage 3 help twice in Go: memory *and* GC.

## Self-check — Chapter 4

1. Why can a linear scan of a packed byte array beat a hash lookup for a 20-field hash?
2. What exactly does `used_memory` count in C Redis, and how will DiceMe maintain the equivalent?
3. Why does `fork()` give C Redis a free consistent snapshot? Which kernel mechanism pays for it, and when?
4. Why can't Go fork? What discipline makes "shallow-copy the map" a correct snapshot?
5. Name two ways packed encodings help a *Go* implementation that don't apply to C.

---

# Chapter 5 — The data structures you will build

> ### 📚 Read alongside — 4 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `dict.c` top-of-file comment + `dictRehash`, `dictScan` (read the reverse-binary-iteration essay above `dictScan` — the best comment in the codebase) | 1 h |
> | **[CORE]** | `listpack.c` header comment — the exact byte format, worked through | 45 min |
> | **[CORE]** | Pugh, "Skip Lists: A Probabilistic Alternative to Balanced Trees" (CACM 1990) — `epaperpress.com/sortsearch/download/skiplist.pdf`; short, readable; then `t_zset.c:zslInsert` | 1 h |
> | **[USE]** | `quicklist.h` struct comments; `intset.c` (tiny — read it all) | 30 min |
> | **[OPT]** | `rax.c` header comment (radix tree, used by streams) — skim for Phase 8 | 20 min |

These are built as **standalone libraries with their own tests in Stage 3** (ch. 20), before Phase 2 wires them into commands. This chapter is the theory; ch. 20 is the spec.

## 5.1 SDS — and what replaces it in Go

C strings are null-terminated: O(N) length, no binary safety, buffer overruns. SDS (`sds.c`) prefixes a header {len, alloc} so length is O(1), contents are binary-safe, and append can grow geometrically. Five header sizes (`sdshdr5`…`sdshdr64`) exist to shave header bytes for short strings.

**In Go, `string` and `[]byte` already are SDS** — length-prefixed, binary-safe. You skip the build, but keep two SDS lessons: (a) preallocation policy — an append-heavy value should over-allocate (Go's `append` already doubles; fine); (b) the embstr idea — Redis stores strings ≤44 bytes inside the object header's cache line (one allocation, not two). Your `deduceTypeEncoding` already knows the 44; in Go the analogous trick is storing small strings by value in the object rather than behind a pointer — optional, measured, Phase 8.

## 5.2 The dict: incremental rehash

The main keyspace is a chained hash table (`dict.c`). The one non-obvious mechanism: **rehashing never happens all at once.**

A dict holds **two** tables, `ht[0]` and `ht[1]`, plus `rehashidx`. When load factor crosses 1.0 (forced at 5.0), `ht[1]` is allocated at the next power of two and `rehashidx = 0` — that's the whole "start". From then on:

- Every dict operation first moves one bucket's chain from `ht[0]` to `ht[1]` (`_dictRehashStepIfNeeded`), advancing `rehashidx`.
- `serverCron` additionally moves up to 100 buckets per millisecond-slice (`dictRehash(d, 1000)` bounded by time) so an idle server still finishes.
- During rehash: **lookups check both tables; inserts go to `ht[1]` only** (so `ht[0]` monotonically drains). Delete checks both.
- When `ht[0]` empties: free it, swap, `rehashidx = -1`.

Why: rehashing 100M keys at once is seconds of stall (§1.4). Amortized, it's nanoseconds per op.

**Go decision:** Go's built-in map hides its growth (it *is* incrementally grown internally, but you can't observe or control it, can't iterate stably across growth, and can't implement SCAN's guarantee on it). **Build the dict.** It's ~300 lines, teaches the two-table pattern, and SCAN (§5.3) plus eviction sampling (§8.2) *require* access to buckets. Use Go's map only for internal bookkeeping (watchers, pubsub registries) where SCAN semantics don't apply. Sizing: power-of-two buckets, hash with `maphash` or xxhash, chain with slices or singly-linked entries.

## 5.3 SCAN and the reverse-binary cursor

`KEYS` is O(N) and blocks. `SCAN cursor` returns a page of keys plus the next cursor, doing bounded work per call. The guarantee: **every key present for the entire scan is returned at least once; keys added/removed mid-scan may or may not appear; duplicates are possible.** No guarantee is "no duplicates" — clients must tolerate them.

The problem: the table can *rehash between calls*. A naive 0,1,2,…,N bucket walk breaks — after a resize, visited/unvisited buckets interleave. The solution (`dictScan`): visit buckets in **reverse-binary-increment order** — add 1 to the cursor's *high* bit and carry *downward* (equivalently: reverse the bits, increment, reverse back). Because a table of size 2^n maps bucket `b` to buckets `b` and `b+2^n` when doubled (the hash's low bits index the table), reverse-binary order visits a bucket and *all its future split-images* before moving on. Result: a cursor from any table size remains valid in any other size, with only bounded re-visiting.

Read the long comment above `dictScan` until you can re-derive the argument. Then implement SCAN in Phase 1 with a small table, force a rehash mid-scan in a test, and verify the guarantee holds. This is a done-when.

## 5.4 listpack (and its ancestor ziplist)

A **listpack** is a small collection serialized into one contiguous byte buffer:

```
<total-bytes u32> <num-elements u16> <entry>* <0xFF end>
entry: <encoding+len header (1-5 bytes)> <payload> <backlen (1-5 bytes)>
```

Elements are strings or integers; integers are stored in the smallest fitting width (13-bit, 16, 24, 32, 64 — the encoding byte says which). `backlen` (the entry's own length, written at its *end*) enables backward traversal without offsets. Insert/delete = `memmove` of the tail + header updates: O(N), fine for N ≤ 128ish.

Why it exists: §4.1 — for small N, one allocation and linear scans beat a real structure's pointers, by ~10× memory and often speed. Lists, hashes, sets, and zsets *all* use a listpack while small (§6.2 thresholds), converting to the real structure when they outgrow it.

Ziplist (`ziplist.c`) is the deprecated ancestor — same idea, but entries encoded the *previous* entry's length at their head, so growing one entry could cascade re-encodings through the rest (`O(N²)` worst case, the infamous "cascade update"). Listpack's design (each entry self-describing, backlen at the tail) exists specifically to kill that. Build listpack only; read ziplist's cascade-update comment as a cautionary tale.

**Build in Go**: `type listpack []byte` with Insert/Delete/Get/Iterate — real byte-twiddling, ~500 lines with tests. This is the most "from scratch" artifact in the project; don't cheat with `[]string`, the whole point is the encoding.

## 5.5 quicklist

A **quicklist** is the production list: a doubly-linked list *of listpacks*, each node holding up to `list-max-listpack-size` entries (default 128 / 8 KB). Ends stay unpacked-ish for fast push/pop; middle nodes can be LZF-compressed (`list-compress-depth`) since list access concentrates at the ends. You get: O(1) amortized push/pop at both ends, bounded memmove costs (one node, not the whole list), decent memory.

Build: a plain doubly-linked list of your listpacks, split-on-overflow, merge-on-underflow (adjacent nodes both < half-full). Skip compression (or add LZF in Phase 8).

## 5.6 skiplist

Sorted sets need: insert/delete by member, score lookup by member (that's a companion dict), **range queries by score and by rank**, all at O(log N). Redis uses a skiplist (`t_zset.c:zslInsert`), not a balanced tree, because — antirez's stated reasons — it's simpler to implement/debug, and range iteration is trivial (level-0 is a sorted linked list).

Mechanism: each node gets a random level (level i+1 with probability 0.25, cap 32); level i is a sorted linked list skipping ~4^i nodes; search descends levels. Two Redis twists: nodes carry **span** (how many level-0 nodes each forward pointer jumps) so `ZRANK` is O(log N) — implement spans, most tutorials skip them and then `ZRANK` becomes O(N); and level-0 has **backward pointers** for reverse ranges (`ZREVRANGE`).

Ordering rule: by score, then lexicographically by member for equal scores. The zset = skiplist + `dict member→score`; both point at the same member string. Every mutation touches both; a mismatch is corruption — assert consistency in tests.

## 5.7 intset

A set of only integers, stored as a **sorted array** with a uniform width (16/32/64-bit) that **upgrades** when a bigger int arrives (re-encode the whole array; never downgrades). Membership = binary search. ~150 lines in Go. It's the warm-up build — do it first in Stage 3.

## 5.8 rax (radix tree) — read-only for now

Streams (Phase 8) index entry IDs with a compressed radix tree (`rax.c`); cluster slot→key tracking historically used one too. Skim the header comment; build only if you do streams.

## 5.9 Geohash — how GEO commands are a zset in disguise

Redis's geospatial support (`GEOADD`, `GEOSEARCH`, `GEODIST`…) adds **no new storage type**. `GEOADD sf 13.361389 38.115556 "Palermo"` stores into an ordinary sorted set whose *score* is a **52-bit geohash**: latitude and longitude are each binary-subdivided 26 times (is it in the left or right half of the range? emit a bit, halve the range — `geohash.c:geohashEncode`), and the two bit-strings are **interleaved** (lng, lat, lng, lat…) into one integer. That interleaving is a **Z-order / Morton curve**: it maps 2-D space onto a 1-D line such that numerically-close scores are (usually) geographically-close cells — which is exactly what a zset can range-query.

Why 52 bits: an IEEE-754 double's mantissa holds 52 bits exactly, so the geohash survives as a zset score with zero loss. Precision at 26 subdivisions ≈ 0.6 m — plenty.

A radius/box query (`GEOSEARCH … BYRADIUS 200 km`) then works like this (`geohash_helper.c`):

1. Pick the deepest geohash cell size that still covers the radius (fewer bits = bigger cell).
2. Take the cell containing the center **plus its 8 neighbors** (the 3×3 block — needed because the center may sit near a cell edge, and because the Z-curve has seams where numeric adjacency lies about spatial adjacency).
3. Each of the 9 cells is a contiguous **score range** (the cell's bit-prefix padded with 0s … padded with 1s) → 9 zset range scans.
4. Filter candidates by *exact* haversine distance (the cells over-approximate).

The lesson generalizes: **map a query shape onto a structure you already have** — 2-D proximity became 9 range scans on a 1-D ordered index. The same trick (space-filling curves over B-trees) powers geo-indexes in MySQL, DynamoDB, and MongoDB. Build it in Phase 8 (menu row in ch. 27): the encode/decode + neighbors math is ~300 lines and the rest is zset calls you already own.

> 📚 Wikipedia: "Geohash" and "Z-order curve" (`en.wikipedia.org/wiki/Geohash`, `en.wikipedia.org/wiki/Z-order_curve`) · G.M. Morton, *A Computer Oriented Geodetic Data Base* (1966) — the original · `geo.c:geoaddCommand`, `geohash.c:geohashEncode`, `geohash_helper.c` in the redis source.

## Self-check — Chapter 5

1. During incremental rehash, why do inserts go only to `ht[1]`? What invariant would double-inserting break?
2. State SCAN's exact guarantee. Why does bucket order 0,1,2,… break it under resize, and what property of reverse-binary order fixes it?
3. In listpack, why is the entry length written at the entry's *end* too? What did ziplist do instead, and what disaster followed?
4. Why a skiplist over a red-black tree, per antirez? What does the span field buy?
5. Why does intset upgrade but never downgrade?
6. For each structure — dict, listpack, quicklist, skiplist, intset — name the command family that dies without it.

---
---

# PART II — MECHANISMS

---

# Chapter 6 — The object model and the keyspace

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `object.c`: `createObject`, `tryObjectEncoding`, `objectCommand` | 45 min |
> | **[CORE]** | `db.c`: `lookupKeyReadWithFlags`, `dbAdd`, `setKey` — the keyspace API every type file calls | 45 min |
> | **[OPT]** | `server.h`: the `robj` struct + the `OBJ_*` / `OBJ_ENCODING_*` defines — your `core/object.go` is a port of this; see what you left out | 20 min |

## 6.1 robj: one header for every value

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
- **refcount** exists in C for shared objects and memory management. In Go the GC handles lifetime — **skip refcount** (your snapshot discipline in §22.3 replaces its one load-bearing use). C Redis pre-creates the integers 0–9999 as shared objects (`shared.integers`) so every `:5` reply and small value costs nothing; in Go, interning small-int strings is an optional Phase 8 micro-optimization.
- **lru** — 24 bits holding either the LRU clock (seconds, wraps ~194 days — your `getCurrentClock` already masks to 24 bits) or, under LFU policy, 16 bits of decay-time + 8 bits of log-counter (§8.3). Same field, two interpretations, chosen by `maxmemory-policy`.

## 6.2 The encoding matrix — learn it cold

| Type | Small encoding | Threshold (config, defaults) | Large encoding |
|---|---|---|---|
| string | `int` (is an integer), `embstr` (≤44 B, one allocation) | — | `raw` |
| list | `listpack` | `list-max-listpack-size` 128 | `quicklist` |
| hash | `listpack` (field,value,field,value…) | `hash-max-listpack-entries` 128, `-value` 64 B | `hashtable` (dict) |
| set | `intset` (all ints) → `listpack` | `set-max-intset-entries` 512, `set-max-listpack-entries` 128 | `hashtable` (dict) |
| zset | `listpack` (member,score…) | `zset-max-listpack-entries` 128, `-value` 64 B | `skiplist` + dict |

Conversion rules: **one-way** (never convert back down when items are removed — avoiding thrash beats reclaiming bytes), triggered on the *insert or growth* that crosses either the entry-count or the value-length threshold. `OBJECT ENCODING key` exposes the current encoding — it's how your tests prove conversions happen at exactly the configured boundary (a Phase 2 done-when).

`tryObjectEncoding` (object.c) is the string version: try parsing as int → `int` encoding; ≤44 bytes → `embstr`; else `raw`. Your `deduceTypeEncoding` already matches. One subtlety you're missing: **`APPEND` and `SETRANGE` must force `raw`** — mutating an int-encoded or embstr value in place is illegal (embstr is immutable by design; int has nothing to append to). Phase 1.

## 6.3 The keyspace API — one choke point

Every type file (`t_string.c`, `t_list.c`…) goes through `db.c`, never touching the dict directly:

- `lookupKeyRead(db, key)` — lookup **honoring expiry** (§7.2), counting hit/miss stats, updating LRU/LFU. May return NULL for an existing-but-expired key.
- `lookupKeyWrite(db, key)` — same plus expiry-deletion has write semantics (propagates, dirties).
- `dbAdd` / `setKey` / `dbDelete` — mutations, which also: bump the **dirty counter** (feeds RDB save points §9.2 and replication), fire **keyspace notifications** (§13.5), **signal watched keys** (§13.1 — `touchWatchedKey`), and **signal ready keys** for blocked clients (§13.3).

This is the architecture lesson of the chapter: **cross-cutting concerns attach to the keyspace choke point, not to 200 command implementations.** When you add WATCH in Phase 6, it's five lines in `setKey`, not a hunt through every command. Your current code half-has this (`core/store.go`) — Phase 1 formalizes it: *no eval function may touch `store`/`expires` directly.* (`evalKEYS`, `evalRENAME`, `evalPERSIST` currently do.)

## 6.4 One database, or sixteen

Redis has `databases 16` (`SELECT n`) — a legacy feature; Cluster only allows db 0; the world uses db 0. Build a single `redisDb` struct `{dict, expires}` and a `SELECT` that errors for n>0, or make it an array — either is fine. The struct matters (it's the unit replication and persistence iterate); the multiplicity doesn't.

## Self-check — Chapter 6

1. Why are type and encoding separate fields? Which one does `WRONGTYPE` check?
2. Why are encoding conversions one-way?
3. What five cross-cutting concerns hang off the keyspace write path? Where would WATCH live if you skipped the choke point?
4. Why must `APPEND` force `raw` encoding?
5. What are the two interpretations of the 24-bit `lru` field, and what chooses between them?

---

# Chapter 7 — Expiration

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `expire.c`: top-of-file comment + `activeExpireCycle` (the modern one walks ebuckets; read the *algorithm description* in the comment, which still describes the classic sampling contract) | 45 min |
> | **[CORE]** | `db.c:expireIfNeeded` — the lazy path and the replica rule; read the comment block above it three times | 30 min |
> | **[USE]** | `tests/unit/expire.tcl` — expiry semantics as executable spec; plus `redis.io/commands/expire` (the "Appendix: how Redis expires keys" section is the official writeup of this chapter) | 30 min |

## 7.1 The contract

- A key with a TTL in the past **must never be observable** — not by GET, not by EXISTS, TYPE, TTL, RANDOMKEY, SCAN, KEYS. To every reader it is already gone, regardless of whether the delete has physically happened.
- TTLs are **absolute Unix-millisecond timestamps** internally (`expires[key] = now + ttl` — yours already does this). `EXPIRE 10` and `PEXPIREAT <ts>` are two spellings of one storage.
- `SET` on an existing key **discards** its TTL (yours does — correct). `GETSET`/`SET ... KEEPTTL` variations preserve it. `RENAME` carries the TTL. `PERSIST` removes it and reports whether it did.
- `EXPIRE` with a **non-positive TTL deletes the key immediately** — semantically a `DEL`, and it *propagates* as one (§7.4). Audit #7: your `evalEXPIRE` sets a past timestamp instead, leaving a phantom that hangs around until sampled.
- Modern flags — `EXPIRE key sec [NX|XX|GT|LT]` (only-if-none, only-if-some, only-if-greater, only-if-less): Phase 1 stretch, trivial once storage is right.

## 7.2 Lazy expiry: expireIfNeeded

Every keyspace lookup funnels through one function:

```
expireIfNeeded(db, key):
    if no TTL or TTL in future: return VALID
    if server is a REPLICA:     return EXPIRED   // but DO NOT DELETE — §7.4
    delete key (sync or lazy-free)
    propagate DEL/UNLINK to replicas + AOF
    fire "expired" keyspace notification
    return EXPIRED
```

The subtleties that distinguish a toy:

1. **The delete is a write, with full write side-effects** — propagation, dirty counter, notifications, watched-key touch. Your current `Get → hasExpired → Del` silently drops all of those (fine today; fatal the day replication exists — this is *the* classic replication-divergence bug, Appendix F).
2. **One choke point.** GET, TYPE, TTL, EXISTS all pass through it. Your `evalGET`'s dead second check (audit #18) is what happens when expiry logic is sprinkled instead of funneled.
3. Commands that will *write* the key still check it (`lookupKeyWrite` → expired means "doesn't exist", so `SETNX` succeeds).

## 7.3 Active expiry: sampling with a budget

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

Modern note: 8.x replaced sampling with `ebuckets` — TTLs bucketed by deadline, so the cycle pops whole due-buckets instead of sampling. Better, and a good Phase 8 refactor; build sampling first because it teaches the budget discipline.

## 7.4 Expiry × replication — the rule everyone gets wrong

The most important section in this chapter, and per-hour the most valuable in the book:

> **Replicas never expire keys on their own.** The master expires (lazily or actively) and **propagates an explicit `DEL`/`UNLINK`**. A replica's `expireIfNeeded` *hides* an over-due key from readers (returns "gone") but leaves it in memory until the master's DEL arrives.

Why: replica clocks differ from the master's; if each node expired independently, keyspaces diverge — then a failover promotes a replica whose "wrong" extra keys suddenly resurrect, or whose missing keys were still alive on the master. One source of truth for the *fact* of expiry; clock skew reduced to read-visibility jitter.

Two corollaries you will implement in Phase 5:

- The DEL the master propagates is the **canonical event**; AOF gets it too, so replaying the AOF reproduces the exact expiry timeline, independent of replay-time clock.
- **Writable-replica caveat** aside (don't build writable replicas), the replica *serving* a read of an over-due key returns nil while `DBSIZE` still counts it. The asymmetry is by design; write it into your tests.

## Self-check — Chapter 7

1. Why must expiry-on-read take the full write path (propagate, dirty, notify)? Name the failure if it doesn't.
2. Two reasons the active cycle samples `expires` and not the main dict?
3. What does the 25%-rule bound, statistically? What does the time budget bound? Why are both needed?
4. A replica's clock runs 30 s fast. Walk a key with 10 s TTL: what does each node store, hide, and delete, and when?
5. Why does `EXPIRE key -1` propagate as `DEL` rather than as `EXPIRE key -1`?

---

# Chapter 8 — Eviction

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `evict.c`: file comment, `evictionPoolPopulate`, `performEvictions` — compare against your `core/eviction*.go` as you read | 1 h |
> | **[CORE]** | antirez, "Random notes on improving the Redis LRU algorithm" — `antirez.com`; the sampled-LRU design writeup, with the famous scatter plots. Docs companion: `redis.io/topics/lru-cache` | 30 min |
> | **[OPT]** | The LFU section of `evict.c`'s comment (Morris counter + decay) | 20 min |

## 8.1 When eviction runs

Before executing any command that may allocate, `processCommand` checks: `used_memory > maxmemory`? Then `performEvictions` frees until under the limit (or gives up). If the policy is `noeviction` (default!) or nothing is evictable, **write commands are rejected** with `-OOM command not allowed when used memory > 'maxmemory'`; reads still work.

Get the trigger model right: eviction is **on the write path, synchronous, just-enough** — not a background sweeper, not a 40% purge (audit #12). It frees the *minimum* to fit the current need, so a workload hovering at the limit evicts one key at a time forever, which is exactly right.

## 8.2 The eight policies

```
noeviction                      reject writes when full (default)
allkeys-lru / volatile-lru      approximate LRU over all keys / only TTL'd keys
allkeys-lfu / volatile-lfu      approximate LFU
allkeys-random / volatile-random
volatile-ttl                    evict nearest-expiry first
```

`volatile-*` sample from the **expires dict**; if it's empty, volatile policies behave like noeviction. That's the entire difference — same machinery, different sampling source. (Your code samples only `store` — Phase 4.)

## 8.3 Approximate LRU: sample + pool

True LRU needs a linked list threaded through every object — 16 bytes/key and a list update per read. Redis instead:

- Stamps each object's 24-bit `lru` field with the **server-cached clock** (`server.lruclock`, refreshed by cron — reading the time syscall per lookup is too slow; your `getCurrentClock` calls `time.Now()` per access, fine at your scale, cache it when you care).
- On eviction: **sample `maxmemory-samples` (default 5) random keys** from the dict, compute idle time (with wraparound — your `getIdleTime` is already correct), and evict the idlest.

Sampling 5 approximates LRU badly per-decision but well in aggregate; antirez's plots show sample-10 nearly indistinguishable from true LRU. The **eviction pool** (`evictionPoolPopulate`) sharpens it further: a 16-slot array, kept sorted ascending by idle time, that *persists across evictions*. Each round's samples are inserted if idler than something present; eviction takes the idlest end. The pool accumulates the best candidates seen across rounds, so later evictions choose from better-than-random memory.

Pool insertion rules (this is exactly the code your audit-#3 bug is in): find rank by idle time; if the pool is full and the newcomer is less idle than everything, skip it; otherwise insert in position, dropping the **least-idle** occupant. Your version drops the *most*-idle (the best candidate) and corrupts the ordering — Phase 4 rewrites `Push` against property tests.

**Random-sampling a Go map vs the dict:** `for k := range m { break }` is *not* uniformly random and famously biased. Your own dict (§5.2) gives you honest random sampling: pick a random bucket, walk it. Another reason the dict build isn't optional.

## 8.4 LFU: a 4-bit-feel counter in 8 bits

`allkeys-lfu` reinterprets the 24-bit field: 16 bits = last-decay time (minutes), 8 bits = a **logarithmic counter** (Morris counter): on access, increment with probability `1/(counter × lfu_log_factor + 1)` — so 255 represents millions of hits. Decay: counter halves (or -1, config `lfu-decay-time`) per elapsed minute-window, so old fame fades. Eviction: same pool machinery, ranked by counter instead of idle time.

LFU beats LRU when "recently touched once" ≠ "hot" (scans pollute LRU). Build it in Phase 4 — it's ~40 lines once LRU works, and the probabilistic counter is a beautiful trick to own.

## 8.5 What eviction must also do

Each evicted key is a **write**: propagate `DEL`/`UNLINK` to replicas and AOF (replicas must not evict independently — same argument as §7.4, and yes, replicas *don't* run eviction on their own dataset for exactly that reason), fire `evicted` notification, count `evicted_keys` in INFO. And under the engine-goroutine model, eviction is naturally atomic with the write that triggered it — no one observes the over-limit state.

## Self-check — Chapter 8

1. Why is eviction on the write path rather than a background job? What does "just enough" prevent?
2. What makes `volatile-lru` different from `allkeys-lru` in code? One line.
3. Why does the eviction pool improve on pure sample-of-5? What property must its insertion maintain — and state the two ways your current `Push` violates it.
4. How does an 8-bit counter represent a million accesses? Why is decay required?
5. Why don't replicas evict on their own?

---

# Chapter 9 — Persistence: RDB and AOF

> ### 📚 Read alongside — 4 h (read twice)
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "Persistence" — `redis.io/topics/persistence`; the best overview antirez ever wrote, largely still his text | 45 min |
> | **[CORE]** | `rdb.c`: `rdbSaveRio` (the writer loop) + the opcode defines in `rdb.h` | 1 h |
> | **[CORE]** | `aof.c`: file comment (the multi-part AOF design doc), `feedAppendOnlyFile`, `flushAppendOnlyFile` | 1 h |
> | **[USE]** | ch. 29 of this book before building; `tests/integration/aof.tcl` | 45 min |

## 9.1 Two philosophies

| | RDB | AOF |
|---|---|---|
| What's on disk | point-in-time binary snapshot | log of every write command |
| Recovery | load snapshot | replay log |
| Loss window | since last snapshot (minutes) | per fsync policy (0–1 s) |
| File size / load time | compact / fast | large / slow (until rewrite) |
| Runtime cost | fork spike, COW memory | small per-write CPU + buffer |

Production commonly runs **both**; on boot, **AOF wins** if enabled (it's more complete). Modern default AOF is *RDB-preamble*: the rewritten base is an RDB file, with RESP commands appended after — you get RDB's load speed and AOF's completeness. You will build: RDB fully, AOF fully, rewrite with RDB preamble as the final step.

## 9.2 RDB

**Format** (build your own, one byte at a time — this is the point):

```
"REDIS0011"                       magic + version
AUX fields (0xFA): redis-ver, bits, ctime, used-mem…
0xFE <dbnum>                      SELECTDB
0xFB <dict-size> <expires-size>   RESIZEDB hint
per key:
  [0xFC <ms-timestamp u64le>]     optional EXPIRETIME_MS
  <value-type byte>               0=string 1=list… (encoding-specific variants)
  <key: string-encoded> <value: type-specific encoding>
0xFF                              EOF
<CRC64 u64le>                     checksum of everything before
```

String encoding within RDB has its own micro-format (`rdbSaveLen`): length in 6, 14, or 32/64 bits chosen by the top 2 bits of the first byte — plus special forms for int-as-string and LZF-compressed. Implement the 6/14/32-bit forms and the int forms; skip LZF (flag a config to refuse compressed files on load).

Collections serialize either as element streams or — key insight — **packed encodings dump their raw bytes**: a listpack-encoded hash is written as one string (the listpack buffer itself). Loading re-validates and possibly re-converts. This makes RDB fast *and* teaches why the encodings being self-contained byte arrays pays off twice.

**When RDB happens:** explicit `SAVE` (blocking — build it first), `BGSAVE`, automatic save points (`save 900 1 / 300 10 / 60 10000` — "N changes in M seconds", checked in serverCron against the **dirty counter**), and on `SHUTDOWN`. The dirty counter increments on every effective write — wire it into the §6.3 choke point now.

**The fork trick** (C): `BGSAVE` forks; the child serializes its frozen COW view; the parent tracks `child_pid`, refuses concurrent children, and reports progress via a pipe (`childinfo.c`). COW cost: every parent-side page write during the save duplicates a 4 KB page — worst case doubles memory. This is why "why did my Redis OOM during BGSAVE at 60% memory" is a canonical ops interview question.

**The Go build** (§4.4, spec in §22.3): snapshot = shallow map copy + replace-don't-mutate discipline; serializer runs on a goroutine over the frozen copy. Same observable semantics: point-in-time, non-blocking (except the bounded copy pause), one at a time.

## 9.3 AOF

Three moving parts, in write order:

1. **feed** — after an effective write executes, append its *propagated form* (§9.4) as a RESP array to an in-memory `aof_buf`. Not the socket bytes the client sent — the canonical rewritten command.
2. **flush** — in `beforeSleep` (once per loop iteration, batching all commands of that iteration), `write()` `aof_buf` to the file. One write per iteration, not per command.
3. **fsync** — per `appendfsync` policy:
   - `always`: fsync in the flush, before replies are sent. Loss window: 0 acknowledged writes. Cost: ~1 ms per iteration — throughput dies; ~200× slower. Correctness note most implementations miss: **the reply must not reach the client before the fsync covering its write completes** — otherwise "always" is a lie.
   - `everysec` (default): fsync on a **background thread** (`bio.c`) at most once per second. Loss window ≤ ~1 s. Extra rule: if the previous background fsync is still running (slow disk), the flush *stalls the write()* rather than queueing unboundedly — backpressure, not memory growth.
   - `no`: let the OS decide (~30 s). Fastest, largest window.
4. **load** — on boot, if AOF enabled: read the file, parse RESP commands, execute them through a **fake client** (a client struct not attached to a socket — build this concept; it returns in replication §10 and scripting). Handle the torn tail: a crash mid-write leaves a truncated final command; `aof-load-truncated yes` (default) logs and drops the tail rather than refusing to start. Your Phase 3 tests kill -9 mid-write and assert exactly this.

**Rewrite** (`BGREWRITEAOF` — the real one): the log grows forever (`INCR x` × 1M = 1M lines for one key). Rewrite generates the *minimal* log: serialize current state (as RDB preamble, or as SET/RPUSH/HSET commands — build commands first, preamble later). While rewriting from a snapshot, **new writes keep appending to the live AOF and are also buffered**; on completion, swap files atomically (temp + rename, §29.2) and splice the buffered tail. C Redis ≥7 does this with a **manifest**: `appendonlydir/` holding a base file + incremental files + a manifest listing them; the swap becomes "write new base + new incr + new manifest" with no giant splice. Build the single-file version; read `aof.c`'s comment to see why the manifest exists (the old splice had a corruption window).

Your audit-#4 file is a rewrite-only AOF opened with `O_APPEND` and no load: after this chapter you can name each of its five sins precisely.

## 9.4 Propagation: effects, not intentions

What goes into the AOF (and to replicas — same stream) is not always what the client typed:

| Client sent | Propagated |
|---|---|
| `SET k v EX 10` | `SET k v PXAT <absolute-ms>` — replay much later must not extend the TTL |
| `EXPIRE k 10` | `PEXPIREAT k <absolute-ms>` |
| `INCRBYFLOAT k 1.1` | `SET k <result>` — float arithmetic must not re-run (platform drift) |
| `SPOP k` | `SREM k <the-popped-member>` — randomness must not re-run |
| GET-path expiry | explicit `DEL`/`UNLINK` (§7.4) |
| `LPUSH` (nothing happened, wrong type) | nothing — only *effective* writes propagate |

The principle: **the log records deterministic effects.** Randomness, clocks, and floats are resolved once, on the master, at execution time. `alsoPropagate`/`propagateNow` (server.c) is the C mechanism for commands that propagate something other than themselves. Design your command signature for this from Phase 1: an eval returns `(reply, effect)` where effect defaults to "propagate verbatim" but can be overridden — retrofitting this in Phase 5 hurts.

## 9.5 Snapshot-without-fork: the Go design problem

Worked options (recommendation: **B**):

- **A. Stop-the-world.** Engine pauses, serialize everything, resume. Simple, correct, seconds of stall per GB. Keep as `SAVE` (which is *supposed* to block); unacceptable for `BGSAVE`.
- **B. Copy-the-index + immutable values.** Pause only to shallow-copy the keyspace map (and expires map) — O(keys) pointer copies, ~100 ms per million keys, tunable by copying incrementally per-dict-bucket. Discipline: **no in-place value mutation after snapshot** — strings already comply (replace `*Obj`); collections comply if mutations copy-on-write the specific node/buffer being changed *while a snapshot is live* (a global `snapshotActive` flag checked by mutators; listpacks copy the buffer, dict-based values copy the entry chain or the whole small dict). Serializer walks the frozen index at leisure.
- **C. Full versioning/MVCC.** Every mutation writes a new version; snapshots pin a version. Strictly more general (gives you consistent SCAN too), strictly more work. This is the ConsulMe/immutable-radix design — you'll appreciate the contrast.
- **D. Subprocess with pipe.** Serialize by streaming *commands* to a helper process that writes the file — doesn't solve consistency, just offloads I/O. Not sufficient alone.

B's trap to test for: a snapshot is live, a client runs `LPUSH biglist x` — assert the serializer sees the pre-push list. Write that test before the implementation; it fails against naive in-place push, and that failure is the lesson.

## 9.6 The crash matrix

Build the table, then make tests enforce every row (Phase 3 done-when):

| Config | `kill -9` at T | After restart |
|---|---|---|
| RDB only, last save T-60s | | everything since T-60s gone |
| AOF everysec | | ≤1 s of acknowledged writes gone; torn tail dropped cleanly |
| AOF always | | zero *acknowledged* writes gone (reply-after-fsync!) |
| AOF + rewrite in flight | | old AOF intact — rewrite must be invisible until the atomic rename |
| RDB + AOF both on | | AOF loaded, RDB ignored |

## Self-check — Chapter 9

1. Why does AOF flush once per event-loop iteration instead of per command? What batches?
2. State the loss window and the mechanism for each fsync policy. What extra ordering rule makes `always` honest?
3. Why is `SPOP` propagated as `SREM`? Give a divergence scenario if it weren't.
4. Why does BGSAVE double memory in the worst case in C? What's the analogous cost of strategy B in Go?
5. Walk the rewrite: where do concurrent writes go, and what makes the swap safe at every instant?
6. Boot with both RDB and AOF present: which loads, and why that one?

---

# Chapter 10 — Replication

> ### 📚 Read alongside — 5 h (read before Stage 7, re-read halfway through)
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "Replication" — `redis.io/topics/replication`; full read | 1 h |
> | **[CORE]** | `replication.c`: the file comment, `syncCommand`, `masterTryPartialResynchronization`, `syncWithMaster` (the replica-side state machine — the longest function you'll read in this project) | 2.5 h |
> | **[USE]** | `tests/integration/replication-psync.tcl` — partial-resync spec | 45 min |

## 10.1 The model

- **Asynchronous.** Master replies to the client, *then* streams to replicas. An acknowledged write can die with the master. (`WAIT numreplicas timeout` lets a client demand N acks — synchronous replication on request, still not consensus: no rollback, no election safety.)
- **Replicas are read-only** (`replica-read-only yes`) and can serve stale reads; `INFO replication` exposes lag.
- **Chained**: a replica can have sub-replicas; it forwards the master's stream verbatim (it does *not* re-generate it — important: sub-replicas need the master's exact byte stream, offsets included).
- One master per replica; `REPLICAOF host port` switches (and `REPLICAOF NO ONE` promotes).

## 10.2 The three identifiers

Everything in replication reduces to bookkeeping of three values:

- **Replication ID** (`replid`) — 40 hex chars naming a *history*. A master mints one at boot; a promoted replica mints a new one (it's starting a new history) but remembers the old as `replid2` for a grace window (so siblings that followed the old master can still partially sync).
- **Offset** — byte count into that history's command stream. Master increments as it writes the stream; each replica tracks the offset it has applied, and reports it back (`REPLCONF ACK <offset>`, every second — feeds `WAIT` and `min-replicas-to-write`).
- **Backlog** — one circular buffer (default 1 MB, size it in config) on the master holding the most recent stream bytes, allocated when the first replica connects. Disconnection ≤ backlog-window → partial resync.

`ReplID + offset` is a **position in a replicated log** — Raft's (term, index) with the safety guarantees removed. Seeing that mapping is half of what this stage teaches; the other half is what its absence costs (§10.6).

## 10.3 The handshake, byte by byte

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

## 10.4 The steady state

After sync, the master treats the replica as *a client that receives but never asks*: every effective write's propagated form (§9.4 — the same rewritten stream the AOF gets) is appended to each replica's output buffer and to the backlog, offset advancing per byte (`replicationFeedSlaves`). Replica applies commands through the fake-client path (no replies generated, `flags |= CLIENT_MASTER`), tracks its applied offset, ACKs each second.

Two periodic frames ride the same stream: master pings replicas (`repl-ping-replica-period 10`) so idle links advance offsets and prove liveness; replicas that miss ACKs long enough are dropped (`repl-timeout`).

**The buffer-limit interaction** (§28.4): a slow replica's output buffer grows on the master; past `client-output-buffer-limit replica 256mb 64mb 60`, the master kills the link — the replica reconnects and (hopefully) partial-syncs from the backlog. Under-size both and you get the classic **full-resync loop**: slow replica → buffer kill → reconnect → full sync (heavy) → even slower → repeat. You will reproduce this on purpose in Phase 5's chaos test, because recognizing it in the wild is worth an entire ops-year.

## 10.5 Where replication hooks the loop

- Feed: inside command propagation (same call site as AOF feed — one `propagate()` fans to both).
- ACK processing / timeout checks / reconnects: `replicationCron` (1/sec, from serverCron).
- Sending: same output-buffer machinery as normal clients, drained in `beforeSleep`.

Under your engine-goroutine model: the engine owns feed + backlog; per-replica writer goroutines own their sockets; `replicationCron` is a case in the engine's ticker. No new concurrency appears — replication is *bookkeeping*, not parallelism.

## 10.6 What asynchrony costs — know it cold

- Master accepts `SET k 1`, acks, dies before feeding replicas. Failover promotes a replica. `k=1` is gone *after acknowledgment*. Redis's position: acceptable for its domain; use `WAIT` or use a CP system when not.
- **Split brain**: old master isolated with clients still writing to it; failover promotes a replica; partition heals; old master is demoted and **wipes its divergent writes** (it full-resyncs from the new master, replacing its dataset — the writes made during the partition are simply destroyed). `min-replicas-to-write N` + `min-replicas-max-lag` shrinks this window (a master that can't reach N fresh replicas refuses writes) — it's a mitigation, not a proof.
- Compare with Raft (ConsulMe ch. 8): Raft refuses the ack until a quorum has the entry. Redis takes the ack-first path deliberately: this *is* the CAP trade made concrete in two adjacent projects on your disk.

## Self-check — Chapter 10

1. What triple identifies a stream position? Map each element to its Raft cousin and name the guarantee Raft adds.
2. Exactly which two conditions admit +CONTINUE? Which config sizes the window?
3. During full resync, where do writes-executed-during-BGSAVE go, and what property does that queue guarantee about the replica's final state?
4. Why must a promoted replica change its replid? Why keep replid2?
5. Describe the full-resync loop failure and both configs implicated.
6. Why does a chained sub-replica need the master's *verbatim* stream rather than a re-generated one?

---

# Chapter 11 — Sentinel

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "High availability with Redis Sentinel" — `redis.io/topics/sentinel`; full read; the "consistency under partitions" example especially | 1 h |
> | **[USE]** | `sentinel.c`: `sentinelCheckObjectivelyDown`, `sentinelStartFailover`, and the failover state machine around `sentinelFailoverStateMachine` | 45 min |

Sentinel is the HA layer for non-cluster Redis: N sentinel processes (run ≥3, odd) monitor masters, agree on failure, and orchestrate promotion. It is a *separate process* reusing the redis binary (`redis-sentinel`), and it is Phase 8 stretch — but read this now, because it completes the replication story and contrasts beautifully with ConsulMe.

## 11.1 Detection: SDOWN vs ODOWN

- Each sentinel pings everything (masters, replicas, other sentinels). No pong past `down-after-milliseconds` → that sentinel marks **SDOWN** (subjectively down) — one node's opinion, ConsulMe's "suspect."
- For a *master*, a sentinel asks peers (`SENTINEL is-master-down-by-addr`); **quorum** agreeing → **ODOWN** (objectively down). Only ODOWN can trigger failover. SWIM reached the same shape (indirect confirmation before conviction) for UDP probes; convergent evolution.

Sentinels discover each other and the replica topology with zero config beyond the master: they publish to `__sentinel__:hello` on the monitored master's pub/sub every 2 s (self + master coords + config epoch) and INFO the master for replicas.

## 11.2 The election and the failover

Before acting, the sentinel that saw ODOWN asks peers to vote it **leader for this failover, in this epoch** — majority of *all* sentinels required (not just quorum), one vote per sentinel per epoch, first-come-first-served. It is Raft's election vocabulary (epochs, majority, single vote) applied to a *task lease*, not a log — after the failover, no leader persists.

The leader then (state machine in `sentinel.c`):

1. **Selects** the promotion target: exclude SDOWN/disconnected replicas, prefer `replica-priority`, then highest replication offset (least data loss), then lexicographic runid (determinism).
2. Sends it `REPLICAOF NO ONE` → waits for its INFO to say master.
3. Reconfigures other replicas: `REPLICAOF <new master>`.
4. Bumps the **config epoch**; broadcasts the new map via hello messages. Higher epoch wins all future conflicts — the old master, on return, receives `REPLICAOF` from any sentinel with the newer epoch and demotes (losing its divergent writes — §10.6).

Clients discover the current master by asking any sentinel (`SENTINEL get-master-addr-by-name`) and re-ask on disconnect — client libraries implement this; your Phase 8 build includes a 50-line Go client that does.

## 11.3 What Sentinel is not

Not consensus over data — only over *who is master*; the data-loss windows of ch. 10 all remain. Quorum < majority is legal for detection but the leader vote always needs majority (so a minority partition can *see* ODOWN but never act). Two sentinels on two nodes is worse than one: no majority when either dies. These are exactly the FLP/CAP corners ConsulMe taught — here they're configuration pitfalls instead of theorems.

## Self-check — Chapter 11

1. SDOWN vs ODOWN — and why does replica-SDOWN never trigger anything by itself?
2. Detection needs quorum; acting needs majority. Why the stronger requirement to act?
3. Rank the replica-selection criteria and justify the offset one.
4. What single number decides who wins when two failovers race? Where did you meet this idea in ConsulMe?
5. Why is 2 sentinels worse than 1?

---

# Chapter 12 — Cluster

> ### 📚 Read alongside — 4 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | Redis docs: "Scale with Redis Cluster" (`redis.io/topics/cluster-tutorial`) + "Redis cluster specification" (`redis.io/topics/cluster-spec`) — the spec is the assignment | 2 h |
> | **[USE]** | `cluster_legacy.c`: `clusterProcessPacket` (the gossip switchboard), `clusterHandleSlaveFailover`; `cluster.c:getNodeByQuery` (the redirect decision) | 1.5 h |

Cluster answers a different question than Sentinel: not "who replaces the master" but **"how do 10 masters share one keyspace"** — sharding first, HA integrated.

## 12.1 Slots

The keyspace is split into **16384 slots**: `slot = CRC16(key) mod 16384`. Every master owns a set of slots; ownership is the unit of resharding. Why 16384 and not 2^32: the cluster gossip carries each node's slot set as a **2 KB bitmap** (16384/8); more slots = fatter heartbeats for no benefit at realistic node counts (≤1000).

**Hash tags**: if the key contains `{…}`, only the braced part hashes — `user:{42}:profile` and `user:{42}:cart` share a slot. This is the *only* multi-key mechanism in cluster: multi-key commands (MSET, SINTERSTORE, MULTI batches, Lua) require all keys in **one slot**, else `-CROSSSLOT`. Design consequence worth internalizing: cluster mode moves the join problem to the client's key schema.

## 12.2 Redirects: MOVED and ASK

Any node answers any query — with a redirect if it doesn't own the slot:

- `-MOVED 3999 10.0.0.7:6379` — "slot 3999 permanently lives there." Client updates its slot→node cache and retries. Smart clients (`redis-cli -c`, every production library) bootstrap the full slot map via `CLUSTER SHARDS`/`SLOTS` and go straight to the right node thereafter.
- `-ASK 3999 10.0.0.8:6379` — "for *this key, this once*, ask there" — the migration protocol (§12.4). Client retries at the target prefixed with `ASKING`, and does **not** update its map.

The server side is one function: `getNodeByQuery` — compute slot(s), verify single-slot, check ownership/migration state, return node-or-error. Your Phase 7 port of it is ~80 lines and the heart of the phase.

## 12.3 The cluster bus

Node-to-node runs on a second port (client port + 10000), speaking a **binary protocol** (not RESP): fixed header (`clusterMsg`: sender id/epochs/slot bitmap/flags) + typed body. Message types: PING/PONG (gossip carriers), MEET (join), FAIL (conviction broadcast), PUBLISH (cluster-wide pub/sub), FAILOVER_AUTH_REQUEST/ACK (election votes), UPDATE (stale-config correction).

Gossip: each node pings a few random peers per second (plus any peer silent > timeout/2); every PING/PONG **piggybacks a sample of the sender's view of other nodes** (address, flags, last-pong). Failure detection is ConsulMe ch. 6 vocabulary re-spelled: no pong > `cluster-node-timeout` → mark **PFAIL** (≈suspect); gossip accumulates others' PFAIL reports; majority-of-masters agreement → **FAIL** (≈dead), broadcast immediately. You built exactly this once — the Phase 7 gossip is your SWIM toy with a slot bitmap bolted on.

## 12.4 Resharding

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

## 12.5 Failover inside cluster

Sentinel-less: replicas of a failed master run the election themselves.

1. Replica sees its master FAIL. Eligibility: its link freshness (`cluster-replica-validity-factor` × node-timeout) — a too-stale replica won't self-promote.
2. Waits a **rank-staggered delay** (500 ms + random + rank×1s, rank = position by replication offset — freshest first) — better replicas ask first; a poor-man's leader-preference, not a safety rule.
3. Broadcasts FAILOVER_AUTH_REQUEST with a bumped `currentEpoch`. **Masters** (only masters vote; only one vote per epoch; only if the requester's slots' `configEpoch`s aren't stale) reply ACK.
4. **Majority of masters** → replica promotes: new `configEpoch` (now the highest), claims the slots, broadcasts PONG. Old master on return sees higher epoch on its slots → demotes to replica.

Same skeleton as Raft's election (epoch, one-vote, majority) but guarding **slot ownership**, not a log — data loss windows of §10.6 persist (async replication underneath is unchanged). `cluster-require-full-coverage`: by default, *any* slot uncovered → whole cluster refuses queries (surprising, frequently disabled; know both behaviors).

Epoch collisions (two masters claiming the same configEpoch, possible after resharding races) are resolved by a deterministic rule: the node with the smaller ID bumps — read `clusterHandleConfigEpochCollision`.

## 12.6 What you build in Phase 7

Full slots/MOVED/ASK/resharding + gossip + FAIL conviction + the replica election, with real config epochs. Skipped: the binary wire format byte-compatibility (your bus can be your own encoding — the *protocol logic* is the lesson), cluster pub/sub, and `cluster-allow-reads-when-down` refinements. A 6-node local cluster surviving `kill -9` of a master, with `redis-cli -c` (against your ports) following redirects — that's the exit demo.

## Self-check — Chapter 12

1. Why 16384 slots? What message field does the number size?
2. MOVED vs ASK — state what each changes in the client, and why migration *needs* the weaker one.
3. Which nodes vote in a failover election, what are the three gates on granting a vote, and what does majority-of-masters protect against that quorum-of-anyone wouldn't?
4. Where exactly is the atomic step in resharding a slot?
5. A partitioned minority holds master M for slot 7. Clients write to M. The majority elects M's replica. Heal. Trace M's dataset, step by step.
6. Your SWIM toy from ConsulMe: name two things cluster gossip adds and one SWIM refinement (from Lifeguard) it lacks.

---

# Chapter 13 — Transactions, blocking, pub/sub, scripting

> ### 📚 Read alongside — 3 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `multi.c` — all of it, it's short; `blocked.c`: file comment + `blockForKeys` / `handleClientsBlockedOnKeys` | 1.5 h |
> | **[CORE]** | Redis docs: "Transactions" (`redis.io/topics/transactions` — the WATCH/CAS section) + "Pub/sub" (`redis.io/topics/pubsub`) | 45 min |
> | **[OPT]** | `script.c` file comment — the effects-replication rationale | 20 min |

## 13.1 MULTI/EXEC/WATCH

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
- WATCH registers (client, key) in `db->watched_keys` (a dict key→list-of-clients). **Every keyspace write** calls `touchWatchedKey` — the §6.3 choke point — flagging every watcher `CLIENT_DIRTY_CAS`. EXEC checks the flag: dirty → null reply, clean → execute the queue as one uninterruptible batch. This is optimistic concurrency control — check-and-set at the keyspace level. (Subtlety your tests must cover: a key that *expires* between WATCH and EXEC also touches watchers — expiry is a write, §7.2.)
- EXEC of N commands propagates wrapped in MULTI/EXEC so replicas/AOF apply it atomically too.
- Under the engine-goroutine model, "uninterruptible" is free: EXEC is one engine message that runs N evals before the next message is received.

## 13.2 Why no interactive transactions

Queued commands can't read (you get `+QUEUED`, not results), so no read-modify-write *inside* a transaction. That's what WATCH (optimistic loop: WATCH→GET→MULTI→SET→EXEC→retry-on-null) and Lua (§13.4, read-modify-write server-side) are for. An interactive transaction would hold the single thread hostage mid-transaction across client round trips — architecturally impossible by design. This question ("how do I do read-then-write atomically in Redis?") is a canonical interview probe; you now have the three-part answer.

## 13.3 Blocking commands

`BLPOP k 5`: if `k` has an element — normal LPOP. Else the client **blocks** (the *client*, never the server): flagged blocked, registered in `db->blocking_keys` (key→list of waiting clients), removed from normal read processing; the loop spins on serving everyone else.

Wake-up is the elegant part — **no polling**:

- Any write that makes a key ready (`RPUSH k`, or a RENAME creating `k`…) calls `signalKeyAsReady` — the choke point again — appending to a server-level `ready_keys` list.
- **After the current command (and its full MULTI batch, and its propagation) completes**, `handleClientsBlockedOnKeys` walks `ready_keys`, matching FIFO-blocked clients to available data.
- Ordering rules your tests must pin: waiters served FIFO (first blocked, first popped); the push and the pop are *separate* propagated events (replica sees `RPUSH` then `LPOP` — a blocked BLPOP is propagated as the LPOP it eventually performs); a `MULTI{RPUSH; LPOP}` batch never wakes a blocked client with data it already consumed (wake-up strictly after the whole batch — this ordering is why the hook appends to a list processed later rather than waking inline).
- Timeout: `beforeSleep`/cron scans blocked clients past deadline → null reply.

Family: BLPOP/BRPOP/BLMOVE, BLMPOP, ZSET variants (BZPOPMIN…), XREAD BLOCK, and WAIT (blocks on replica ACK-offset instead of a key). Build BLPOP + WAIT; the rest are the same plumbing.

In Go: a blocked client's connection goroutine simply parks on its reply channel; "blocked" is a registry entry in the engine. No goroutine gymnastics — the registry *is* the mechanism, same as C.

## 13.4 Pub/sub

Fire-and-forget fan-out, entirely orthogonal to the keyspace: `SUBSCRIBE ch` (registry channel→clients), `PUBLISH ch msg` (`pubsubPublishMessage`: push to every subscriber's output buffer *now*), `PSUBSCRIBE news.*` (pattern list, matched per publish — O(patterns), unavoidable). No buffering, no replay, no acks: a subscriber that was disconnected missed the message, full stop (contrast Streams, Phase 8, which is the durable variant). Subscribed clients in RESP2 enter a restricted mode (only [P]SUB/UNSUB/PING allowed); RESP3 lifts this via push frames. Messages to replicas: PUBLISH propagates through the replication stream, so subscribers on replicas hear it. The real cost center is slow subscribers × fan-out = output-buffer blowup → the `pubsub` class of client-output-buffer-limits exists precisely for this (§28.4).

Cluster wrinkle: plain `PUBLISH` must broadcast to **every node** in the cluster (a subscriber could be anywhere) — an O(nodes) bus storm per message. **Sharded pub/sub** (7.0: `SSUBSCRIBE`/`SPUBLISH`/`SUNSUBSCRIBE`, `pubsub.c:spublishCommand`) hashes the channel name like a key: messages route only to the channel's slot-owner shard and its replicas. Same registries, one extra slot check — a Phase 7 stretch once the bus exists.

## 13.5 Keyspace notifications

`notify-keyspace-events` turns writes into pub/sub events on `__keyspace@0__:<key>` (what happened to this key) and `__keyevent@0__:<event>` (which key had this event) — e.g. every `expired`, `evicted`, `set`, `lpush`. Off by default (costs a publish per write). Implementation: one `notifyKeyspaceEvent(type, event, key)` call at — say it with me — the keyspace choke point. Build in Phase 6: it's ~30 lines *because* the choke point exists, and it makes chaos tests observable (`SUBSCRIBE __keyevent@0__:expired` is how you watch your active-expiry cycle work in real time).

## 13.6 Lua scripting in one box (Phase 8)

`EVAL script numkeys k… arg…` runs Lua atomically server-side, calling back via `redis.call`. Semantics you'd need to honor: scripts must declare keys (cluster slot checking), block everything while running (`busy-script` + SCRIPT KILL for read-only scripts), and **propagate by effects** (§9.4 — modern Redis replicates the writes the script performed, not the script text; deterministic-script requirements are the legacy regime). `SCRIPT LOAD`/`EVALSHA` cache by SHA1. Functions (`FUNCTION`, 7.x) are the librarified successor. Go build via a pure-Go Lua (e.g. gopher-lua) — the instructive part is the effects-propagation plumbing, which you'll have from Phase 5.

## Self-check — Chapter 13

1. Queue-time error vs runtime error inside MULTI — different EXEC outcomes. State both and justify the second.
2. Implement WATCH in one sentence given the §6.3 choke point. Why does an *expiry* touch watchers?
3. Why exactly can't Redis offer read-your-queued-writes transactions? What are the two sanctioned substitutes?
4. When precisely does a blocked BLPOP client wake, relative to the RPUSH's MULTI batch and its propagation? Why not inline?
5. What does a replica see when a blocked BLPOP is eventually served?
6. Pub/sub vs Streams in one line each. Which limit protects the server from slow subscribers?

---

# Chapter 14 — Threads in a "single-threaded" server

> ### 📚 Read alongside — 2 h
> | | Resource | Time |
> |---|---|---|
> | **[CORE]** | `bio.c` — all of it (~350 lines): the job queue Redis trusts with fsync | 45 min |
> | **[CORE]** | `lazyfree.c`: `lazyfreeGetFreeEffort` + the UNLINK path | 30 min |
> | **[OPT]** | `networking.c`: the io-threads section (`handleClientsWithPendingReadsUsingThreads`) | 30 min |

## 14.1 The truth table

"Single-threaded Redis" means: **one thread executes commands.** Around it:

| Threads | Job | Why it's safe |
|---|---|---|
| main | event loop + all command execution | owns all data |
| bio ×3 (`bio.c`) | background **fsync**, background **close** (of old AOF/RDB fds — close of a big file can block in the kernel!), background **lazy-free** | operate on handed-off resources the main thread no longer references |
| io-threads ×N (optional, 6.0+) | parse input / write output for batches of clients | main thread stops, threads run on *disjoint clients*, main thread resumes — fan-out/fan-in, never concurrent with execution |
| fork children | RDB save, AOF rewrite | separate process, COW view |

The design rule extractable from all four rows: **threads never share mutable data structures; they receive ownership of work items through a queue.** `bio.c` is a textbook worker pool: mutex + condition variable + job list, jobs like "fsync fd 7" or "free this object graph" whose inputs the main thread has already disowned.

## 14.2 Lazy freeing

`UNLINK` (and `DEL` under `lazyfree-lazy-user-del`, plus eviction/expiry under their lazyfree configs) removes the key from the dict — O(1), the key is *gone* observably — and, if the value's free-effort exceeds a threshold (64), hands the orphaned object to bio for freeing. In Go, **the GC is your lazy-free thread**: drop the reference, done. You inherit this entire mechanism for free — but note *why* it exists, because it's the same §1.4 principle, and note the one place it still bites Go: a multi-GB value's GC work happens *sometime*, as marking cost; if you see latency spikes after mass deletes, you've found it.

## 14.3 io-threads

With `io-threads 4`, the loop batches pending-read clients, wakes threads to read+parse them in parallel, waits, then executes all parsed commands serially on the main thread; symmetrically for writes. Execution stays single-threaded — the speedup is protocol I/O only (worth it when replies are large or clients many). This is fan-out/fan-in with a barrier, not concurrency. Phase 8 stretch in Go: your connection goroutines *already are* io-threads (parsing happens off-engine, in parallel, by construction) — observe that you got 6.0's headline feature for free from the architecture, and say so in the README.

## 14.4 Choosing DiceMe's concurrency model — the decision record

The options, honestly weighed:

**A. Global mutex around the store.** Correct; simple; every command serializes anyway, so it's the engine model with extra steps and worse: blocking commands hold nothing but still need a registry, and you *will* forget the lock somewhere (`Rename`'s eviction reentry — audit #13 — becomes a deadlock candidate).

**B. Sharded locks (16 shards by key hash).** Parallel single-key ops. But: multi-key commands (MSET, SINTERSTORE, RENAME) need multi-shard lock ordering (deadlock discipline); `MULTI` needs *all* touched shards; `RANDOMKEY`/`SCAN`/`FLUSHALL`/`DBSIZE` need all 16; eviction accounting (`used_memory` across shards) races; `WATCH` needs cross-shard coordination. Every mechanism in ch. 7–13 grows a caveat. This is the DragonflyDB path — a legitimate *different* project, and a great "then what" (Closing), but it re-derives every semantic this book teaches.

**C. Engine goroutine (chosen).** Connections parse in parallel (free io-threads); one goroutine owns store+expires+watchers+blocked-registry+repl-backlog and `select`s over {command channel, cron ticker, shutdown}. Every invariant in this book holds by construction. Throughput ceiling: one core of command execution — *which is the real Redis's ceiling too.* When a benchmark saturates it, you've reproduced the real bottleneck; the fix is the real fix (Cluster).

Write this decision into `NOTES.md` in your own words before Phase 0. It's the whole architecture.

## Self-check — Chapter 14

1. The complete list of what bio threads do. Why is *close* on the list?
2. State the ownership rule that makes all Redis threading safe.
3. What replaces lazy-free in Go, and what residual cost remains?
4. Why do io-threads not violate single-threaded execution? Which barrier enforces it?
5. For sharded locks, name four features that grow caveats and the caveat for each.
6. What is the engine model's throughput ceiling, and why is hitting it a feature of the learning project?

---
---

# PART III — REDIS ARCHITECTURE

---

# Chapter 15 — The big picture

## 15.1 The process at rest

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
- **`struct client`** — fd/conn, input buffer + parse resume state (`multibulklen`, `bulklen`), argv, output buffer, flags (`CLIENT_MULTI`, `CLIENT_BLOCKED`, `CLIENT_DIRTY_CAS`, `CLIENT_MASTER`, `CLIENT_SLAVE`…), MULTI queue, watched list, replication ack offset. **A replica is a client. The AOF loader is a client. A Lua call is a client.** The fake-client trick — one execution interface, many drivers — is the architecture's best idea and your Phase 0's `Client` struct should be designed for it from day one.

## 15.2 Boot, in order

`main` (server.c) → parse config → `initServer` (create loop, listeners, dbs, bio threads, register `serverCron` + connection acceptor) → `loadDataFromDisk` (AOF if enabled, else RDB — **before** accepting commands; serving `-LOADING` to early connections) → if replica config present, schedule the ch. 10 handshake → `aeMain`. Phase 0 mirrors this shape exactly; the ordering (load *before* serve) is a done-when.

## 15.3 The two heartbeats

**`serverCron`** — default 10/s (`hz`), the janitor: active expiry cycle (§7.3) · incremental-rehash step (§5.2) · save-point check against dirty counter (§9.2) · `replicationCron` (acks, timeouts, reconnect) (§10.5) · clientsCron (idle timeouts, output-buffer-limit enforcement, input-buffer sanity) · LRU clock refresh · stats rollups. Everything in it must be **bounded per tick** — cron is inside the loop; a slow cron is a slow server (the §7.3 time budget exists exactly for this).

**`beforeSleep`** — every loop iteration, the completer: flush AOF buffer (§9.3) · handle clients newly unblocked (§13.3) · stream replies/replica feed to sockets · fast expire cycle when backlogged. The split rule: **cron does periodic maintenance; beforeSleep completes work the just-finished iteration generated.** Getting a task on the wrong side is a real bug class: AOF flush in cron instead of beforeSleep = up to 100 ms of acknowledged-but-unwritten writes at hz=10.

## 15.4 Layering

```
transport (anet/connection)  →  protocol (networking: parse/reply)
    →  dispatch (server.c: lookup, arity, auth, OOM/loading gates, MULTI queueing)
        →  semantics (t_string, t_list, …: the commands)
            →  keyspace (db.c: lookup/add/delete + choke-point hooks)
                →  structures (dict, listpack, skiplist, quicklist, intset)
cross-cutting, fed by the choke point: propagation (AOF + replication) · notifications
    · watched/ready keys · dirty counter · stats
```

The dependency arrows only point down. `t_list.c` doesn't know AOF exists; `dict.c` doesn't know keys expire. When your Phase 2 code wants to call `feedAOF` from inside a list function, this diagram is the "no."

# Chapter 16 — Request lifecycles, traced

Trace these by hand once each — they are the integration tests of your understanding. Each numbered step names the real function.

## 16.1 `SET k v EX 10`

1. Socket readable → `readQueryFromClient` appends to `c->querybuf`.
2. `processInputBuffer` → `processMultibulkBuffer` extracts `["SET","k","v","EX","10"]` into argv (resumable if partial).
3. `processCommand`: lookup in command table → arity ok → not OOM (or eviction runs, §8.1) → not in MULTI → `call()`.
4. `setCommand` → `setGenericCommand`: parse options; `tryObjectEncoding("v")`; `setKey(db,k,obj)` — choke point: dirty++, touch watchers, notification `set`; `setExpire(db,k,now+10000)`.
5. Propagation: rewritten to `SET k v PXAT <abs>` (§9.4) → `feedAppendOnlyFile` (to aof_buf) + `replicationFeedSlaves` (to backlog + replica buffers).
6. `addReply(c, shared.ok)` → output buffer.
7. …loop iteration ends → `beforeSleep`: aof_buf → write() (fsync per policy); output buffers → sockets. Client reads `+OK`.

Note what step 7 means under `everysec`: the client saw `+OK` before the disk did. That's the documented trade, visible right there in the ordering.

## 16.2 `GET k` on an expired key

1–3 as above → `getCommand` → `lookupKeyReadWithFlags` → `expireIfNeeded`: TTL past → **not a replica** → delete key; propagate `DEL k` to AOF+replicas; notification `expired`; touch watchers → lookup returns NULL → reply `$-1`. One read produced a write; every downstream consumer hears `DEL`, none of them heard "expired by clock."

On a **replica**: same until the branch — key is hidden (return NULL) but not deleted, no propagation; the authoritative `DEL` arrives on the replication stream when the master notices.

## 16.3 `BLPOP q 0` then, elsewhere, `RPUSH q job`

1. Client A: `blpopCommand` → list empty → `blockForKeys(A, [q])`: A flagged blocked, registered in `db->blocking_keys[q]`; **no reply sent**; loop moves on. A's connection goroutine (Go) just waits on its reply channel.
2. Client B: `RPUSH q job` executes fully — list created, dirty++, propagated as RPUSH — and `signalKeyAsReady(db, q)` appends to `server.ready_keys`.
3. After B's command (and any MULTI batch) completes: `handleClientsBlockedOnKeys`: q ready → oldest waiter A → pop `job`, reply to A, **propagate `LPOP q`**.
4. Replica's view: `RPUSH q job` then `LPOP q` — two writes, no blocking anywhere.

## 16.4 `WATCH stock` → `MULTI` → `DECR stock` → `EXEC`, with a race

1. A: WATCH registers (A, stock). A: GET stock → "3". A: MULTI → +OK. A: DECR stock → +QUEUED (not executed).
2. B: `SET stock 7` → choke point → `touchWatchedKey(stock)` → A flagged `CLIENT_DIRTY_CAS`.
3. A: EXEC → dirty flag → **null array**, queue discarded, nothing propagated. A's library retries the whole loop.
4. Without the race: EXEC runs DECR, propagates `MULTI; DECR stock; EXEC` as a unit.

## 16.5 Replica full sync (both sides)

1. R: connect → PING/REPLCONF×2 → `PSYNC ? -1`.
2. M: `syncCommand` → no usable backlog match → `+FULLRESYNC <replid> <offset0>`; start BGSAVE (or attach R to one in flight); R marked WAIT_BGSAVE; **every write from now also queues in R's buffer**.
3. M: child done → stream RDB bytes to R (length-prefixed, or diskless EOF-delimited).
4. R: flush own db → load RDB stream → apply queued writes → steady state: applying feed, `REPLCONF ACK <offset>` each second.
5. M: cron sees acks; `WAIT` queries satisfied; backlog ring keeps last N bytes for R's next hiccup.

## 16.6 Cluster write to the wrong node

1. Client → node X: `SET user:{42}:cart …` → `getNodeByQuery`: slot=CRC16("42")%16384 → owner is Y → `-MOVED 8000 <Y>`.
2. Client updates its slot map, replays against Y → +OK (Y propagates to *its* replicas as ch. 10).
3. If slot 8000 were mid-migration Y→Z and the key absent on Y: `-ASK 8000 <Z>` → client sends `ASKING` + retry to Z, map unchanged.

---
---

# PART IV — READING THE SOURCE

Rules: sessions are keyed to build stages — read each *just before* its phase. Never read more than one session ahead. Every session ends with "what you steal" — the concrete list you port.

## Session 0 — Run it before reading it (Stage 1, 45 min)

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
redis-benchmark -q -n 100000 -P 16                   # your Phase 0 target numbers, on your laptop
```

What you steal: the *behavioral baseline*. Save `redis-benchmark` output to `NOTES.md` — DiceMe's numbers get compared against it at every phase.

## Session 1 — Boot & the three structs (Stage 1, 2 h)

Read: `server.c:main` → `initServer` → `initListeners`; then in `server.h`: `struct redisServer` (skim, flag replication+aof fields), `struct redisDb`, `struct client`, the `CLIENT_*` and `CMD_*` flag blocks; `commands.def` for 30 seconds (it's generated from `commands/*.json` — the command table is *data*).
What you steal: boot ordering (§15.2); the client struct's field list (your `Client` needs ~⅓ of it, but know what you're omitting); command-table-as-data with flags (`write`, `denyoom`, `fast`, arity).

## Session 2 — The loop (Stage 1, 1.5 h)

Read: `ae.c` whole (`aeMain`, `aeProcessEvents`, timer bookkeeping), `ae_kqueue.c`; `server.c:beforeSleep` top-to-bottom (skip TLS/cluster branches, list every job it does), `serverCron` (same treatment).
What you steal: the poll-timeout-from-nearest-timer rule (fixes audit #6); the beforeSleep-vs-cron split (§15.3) as your engine's `select` cases.

## Session 3 — Client I/O and the parser (Stage 1, 2 h)

Read: `networking.c`: `createClient`, `readQueryFromClient`, `processInputBuffer`, `processMultibulkBuffer` (the resume-state fields!), `processInlineBuffer`, `addReply` + `addReplyBulk` + the output-buffer/reply-list machinery, `writeToClient`; skim `handleClientsWithPendingWrites`.
What you steal: the incremental-parse resume state (fixes audit #1/#2); reply-to-buffer-not-socket; protocol-error-then-close discipline.

## Session 4 — Dispatch & propagation (Stage 2, 1.5 h)

Read: `server.c:processCommand` (every gate, in order: exists → auth → cluster redirect → OOM → loading → subscriber-mode → MULTI queue), `call()` (dirty counting, duration, propagation trigger), `alsoPropagate`/`propagateNow`.
What you steal: the gate sequence for your dispatcher; `(reply, effect)` propagation shape (§9.4); dirty counter placement.

## Session 5 — dict (Stage 3, 2 h)

Read: `dict.c`: `dictAddRaw`, `_dictRehashStepIfNeeded`, `dictRehash`, `dictResize` policy, then the `dictScan` comment-essay until you can re-derive it; skim `kvstore.c`'s header comment for the 8.x contrast (per-slot dicts).
What you steal: two-table rehash state machine; rehash-step placement (every op + cron slice); the reverse-binary cursor, verbatim.

## Session 6 — Objects & strings (Stage 2, 1.5 h)

Read: `object.c`: `createStringObject`, `tryObjectEncoding`, `getDecodedObject`, `objectCommandGetKey`; `t_string.c`: `setGenericCommand` (all flags — NX/XX/EX/PX/EXAT/KEEPTTL/GET), `incrDecrCommand` (overflow check!), `appendCommand` (encoding force + grow); `sds.c` skim for the growth policy.
What you steal: SET's full option matrix + error texts; INCR's `ERR increment or decrement would overflow` guard (audit #10); APPEND-forces-raw (§6.2).

## Session 7 — Keyspace & expiry (Stage 2, 1.5 h)

Read: `db.c`: `lookupKeyReadWithFlags`, `lookupKeyWrite`, `setKey`, `dbGenericDelete`, `expireIfNeeded` + its comment, `scanGenericCommand`; `expire.c`: `activeExpireCycle` + `expireGenericCommand` (the NX/XX/GT/LT flags, the delete-on-past branch).
What you steal: the choke-point function set (§6.3); replica-hides-master-deletes (§7.4); the cycle's budget arithmetic; EXPIRE-past→DEL (audit #7).

## Session 8 — Eviction (Stage 6, 1.5 h)

Read: `evict.c`: `getMaxmemoryState`, `performEvictions` (the free-target loop), `evictionPoolPopulate` (insertion order! — this is your audit-#3 fix, read it twice), `LFUGetTimeInMinutes`/`LFULogIncr`/`LFUDecrAndReturn`.
What you steal: pool insertion exactly; just-enough freeing loop; LFU's three tiny functions verbatim.

## Session 9 — RDB (Stage 5, 2 h)

Read: `rdb.c`: `rdbSaveRio` (the main loop), `rdbSaveLen`/`rdbSaveStringObject` (the length micro-format), `rdbSaveObject` for string+hash+zset cases (see packed-encodings-dump-raw), `rdbLoadRio` mirror, opcode list in `rdb.h`; `rdbSaveBackground` + `backgroundSaveDoneHandler` for child lifecycle.
What you steal: your file format (§9.2), byte for byte where convenient; the save-point/dirty logic; child-lifecycle states → your snapshot-goroutine states.

## Session 10 — AOF (Stage 5, 2 h)

Read: `aof.c`: the file-header design comment (manifest rationale), `feedAppendOnlyFile`, `flushAppendOnlyFile` (**the everysec stall branch** — find it, it's the subtlest correctness code in the file), `rewriteAppendOnlyFileBackground`, `loadSingleAppendOnlyFile` (fake client!), `aof-load-truncated` handling.
What you steal: flush/fsync policy machine; fake-client loader; rewrite temp-file+rename choreography; torn-tail handling.

## Session 11 — Replication (Stage 7, 3 h — the long one)

Read: `replication.c` in this order: file comment → `replicationFeedSlaves` (offset+backlog+fanout) → `feedReplicationBuffer` → `syncCommand` → `masterTryPartialResynchronization` (the CONTINUE gates) → `replicaStartCommandStream` → then the replica side: `syncWithMaster` state machine end to end → `readSyncBulkPayload` (RDB receive, diskless EOF mark) → `replicationCron`. Skim `WAIT` impl (`waitCommand`, `replicationRequestAckFromSlaves`).
What you steal: everything. This session is the Phase 5 spec; take notes as pseudocode and the phase becomes transcription.

## Session 12 — Collections (Stage 4, 2.5 h)

Read: `t_list.c`: `pushGenericCommand`, `listTypeTryConversionRaw` (threshold logic); `quicklist.c`: `quicklistPush`, node split/merge; `listpack.c`: `lpInsert` + the format comment; `t_hash.c`: `hashTypeSet` + conversion; `t_set.c`: `setTypeAdd` (intset→listpack→dict ladder); `t_zset.c`: `zslInsert` (spans!), `zslDeleteNode`, `zsetAdd` (the flags matrix NX/XX/GT/LT/CH/INCR), `genericZrangebyscoreCommand`; `intset.c` whole.
What you steal: each conversion trigger, exactly; zslInsert with spans, near-verbatim; ZADD's flag semantics table.

## Session 13 — MULTI, blocking, pubsub (Stage 8, 1.5 h)

Read: `multi.c` whole; `blocked.c`: `blockForKeys`, `handleClientsBlockedOnKeys` (the after-command placement + FIFO), `unblockClient`; `pubsub.c`: subscribe/publish paths; `notify.c` (tiny).
What you steal: dirty-CAS flag mechanics; ready-keys two-phase wakeup; the propagate-the-effective-pop rule; notify's single entry point.

## Session 14 — Cluster & Sentinel (Stage 9, 2.5 h)

Read: `cluster.c`: `getNodeByQuery` (the whole redirect brain); `cluster_legacy.c`: `clusterProcessPacket` (top switch), `clusterSendPing` (gossip-section sampling), `markNodeAsFailingIfNeeded` (PFAIL→FAIL), `clusterHandleSlaveFailover` (election, rank delay, epoch bump), `clusterHandleConfigEpochCollision`; `crc16.c` (your port is 30 lines). Sentinel (only if doing Phase 8's build): `sentinelCheckObjectivelyDown`, `sentinelStartFailover` + the state machine around it.
What you steal: `getNodeByQuery` logic; PFAIL/FAIL conviction rules; election gates; the epoch-collision tiebreak.

---
---

# PART V — IMPLEMENTATION

---

# Chapter 17 — Project setup and conventions

## 17.1 Rules

1. **Zero runtime dependencies** in the server (stdlib only; `gopher-lua` allowed for Phase 8 scripting; test-only deps fine). The point is building it.
2. **Every phase ends green**: its done-when script passes, `go test ./...` passes, `go vet` clean, and the *previous* phases' done-whens still pass (keep them as `make check-phase-N` targets — regressions across phases are the norm, not the exception).
3. **Error texts byte-identical to real Redis.** The diff harness enforces it.
4. **No eval touches storage directly** — everything through the keyspace API (§6.3). Enforced by review, or by making the store package-private with an exported API surface.

## 17.2 Layout (grows from your current tree)

```
diceMe/
├── main.go                    # flags, config load, engine start  (evolves)
├── config/                    # becomes: file parsing + CONFIG GET/SET registry
├── server/
│   ├── engine.go              # NEW Phase 0: the owner goroutine (§14.4)
│   ├── conn.go                # NEW Phase 0: accept loop, per-conn read/parse/write goroutines
│   ├── client.go              # NEW Phase 0: Client struct (buffers, flags, MULTI state, …)
│   ├── async_tcp.go           # kept as the museum piece; no longer wired to main
│   └── sync_tcp.go            # same
├── core/
│   ├── resp/                  # Phase 0: incremental decoder + reply serializer (RESP2, later 3)
│   ├── store/                 # Phase 1: redisDb{dict,expires}, keyspace API, choke point
│   ├── ds/                    # Stage 3: dict/  listpack/  skiplist/  quicklist/  intset/
│   ├── cmd/                   # command table + eval functions per type: string.go list.go …
│   ├── expire/  evict/        # Phase 1 / Phase 4
│   ├── persist/               # Phase 3: rdb.go aof.go snapshot.go
│   ├── repl/                  # Phase 5: master.go replica.go backlog.go
│   └── cluster/               # Phase 7
├── tests/
│   ├── harness/               # §17.4 diff harness
│   └── phaseN_test.sh         # each phase's done-when, executable
└── storm/                     # grows: mixed workloads, chaos scripts
```

Migration note, not big-bang: Phase 0 creates `engine/conn/client` and the `resp` package (porting your decoder's good parts), points `main.go` at them, and *deletes nothing*. Later phases move `core/*.go` contents into their packages as each is rebuilt.

## 17.3 Testing strategy per layer

| Layer | Strategy |
|---|---|
| ds/* | pure unit + property tests (rehash invariants, listpack round-trip, skiplist vs reference `sort` model, SCAN-guarantee test with forced rehash) — plus fuzz (`go test -fuzz`) on listpack and RESP |
| resp | table tests + **fuzz** (must never panic on any byte sequence — audit #1's grave) |
| cmd/store | script tests: run command list against engine in-process, assert replies; then the same list through the diff harness |
| persist | crash tests: `kill -9` the server at randomized points under load (storm), restart, assert the §9.6 matrix row |
| repl/cluster | multi-process integration: spawn N servers on ports, drive with redis-cli, assert convergence; deterministic seeds |

## 17.4 The diff harness — build first, 4 h

The most valuable tool in the project:

```bash
tests/harness/diff.sh commands.txt
#  starts real redis-server :6379 and dice :7379 (fresh, no persistence)
#  pipes each line via redis-cli --no-raw to BOTH; diffs stdout; reports first divergence
```

Plus a Go mode for pipelining/blocking cases (two `net.Conn`s, raw RESP bytes both ways, byte-diff). Every phase's command surface (Appendix B) gets a `commands.txt`. Nondeterminism (RANDOMKEY, SPOP, INFO, TTL-under-latency) is handled by a small allowlist of "normalize before diff" rules — build the allowlist as you hit them, keep it short and documented; every entry is a place your tests are weaker, so prefer seeding determinism (e.g., fixed TTLs, sorted SMEMBERS) where possible.

## 17.5 Tooling (do once, ~1 h)

`Makefile`: `build test vet fuzz check-phase-N bench diff`. `bench` = `redis-benchmark -q -n 100000 -P 16 -p 7379` with the real-Redis column beside it. Pin Go version. `golangci-lint` optional. CI optional (this is a learning repo; the harness is your CI).

---

# Chapter 18 — Phase 0: the server core, rebuilt

**Goal**: DiceMe survives anything a client can throw at the *transport and protocol* layer, with the ch. 14 architecture underneath. No new commands — this phase is plumbing, and it retires audit findings 1, 2, 5, 6, 9, 15, 16, 17.

## 18.1 The pieces

**`resp` package.** Incremental decoder per §3.3: `Decoder{buf []byte; ...}` with `Feed(data)` and `Next() (args [][]byte, err error)` returning `ErrIncomplete` freely. Port your `readX` functions' logic; kill their panics; add `-1` lengths, limits, and inline commands. Serializer side: keep `Encode`-style helpers but emit into a `*bytes.Buffer`/writer, and add the logical-reply layer (§3.7): `ReplyOK/ReplyErr/ReplyBulk/ReplyInt/ReplyArray/ReplyNil` methods on a `RespWriter` — evals call these, never `fmt.Sprintf` RESP by hand.

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

**Command table as data** (Session 1's steal):

```go
var commandTable = map[string]Command{
  "SET": {Eval: evalSET, Arity: -3, Flags: CmdWrite | CmdDenyOOM},
  "GET": {Eval: evalGET, Arity: 2, Flags: CmdReadOnly | CmdFast},
  ...
}
```

Dispatch gates in `processCommand` order (Session 4): unknown → `-ERR unknown command 'x', with args beginning with:` (audit #5 dies) · arity (negative = at-least, Redis convention) · [later: auth, OOM, loading, subscriber-mode, MULTI]. Case-insensitive lookup (uppercase once).

**Connection lifecycle.** Accept goroutine (maxclients gate) → per-conn: reader goroutine (read→Feed→Next loop→engine channel) + writer (drain reply channel→`bufio.Writer`→flush per batch, partial-write-safe by construction). Disconnect cleanup: deregister everything (a function that grows all project long — get its shape right now). Graceful shutdown: signal → stop accepting → engine drains → connections closed → exit; second signal = immediate. `wg` accounting fixed (audit #15).

**Config.** Parse a minimal `dice.conf` (key value lines, `#` comments — Redis's format) + flags override + defaults: port, maxclients, proto-max-bulk-len, appendonly(later), save(later)… Registry pattern so `CONFIG GET/SET` (Phase 1) is a map iteration, and configs are declared once with type+default+validator.

**Commands carried over**: PING, ECHO (new, trivial), SET/GET/DEL/TTL/EXPIRE/etc. keep working through the new path — port the evals as-is; Phase 1 rewrites their semantics. Drop LRU and SLEEP from the table; re-add SLEEP as `DEBUG SLEEP` (build `DEBUG` now with SLEEP + JMAP-ish stub — you'll grow it every phase).

## 18.2 Done-when

```bash
redis-cli -p 7379 PING                          # PONG
redis-cli -p 7379 FOOBAR a b                    # ERR unknown command 'FOOBAR', …
redis-benchmark -p 7379 -q -n 100000 -P 16 -t ping,set,get   # clean, 0 errors — audit #2 dead
printf 'PING\r\n' | nc localhost 7379           # inline works
# partial-frame torture: send a SET split into 1-byte writes with 10ms gaps — correct reply
go test -fuzz=FuzzDecode -fuzztime=60s ./core/resp/          # no panics — audit #1 dead
# 1000 concurrent clients (storm) for 60s; kill client processes randomly; server RSS stable, no goroutine leak (pprof)
# ^C: drains, exits 0.  kill -9 storm mid-run: server unaffected.
# idle server with a 1s-TTL key set → key gone after ~1s (cron runs without traffic — audit #6 dead)
```

## 18.3 Traps

- Deadlock: engine replying into a full client channel while that client's reader is blocked sending to the engine. Rule: writer channels are buffered + engine *never* blocks on them (overflow = kill client, §28.4 semantics arrive early).
- `[][]byte` aliasing: decoder buffers reused → args must be copied (or ownership documented) before crossing the channel. This bug is silent data corruption under pipelining; write the test.
- Goroutine leaks on disconnect — `pprof/goroutine` after the churn test, count must return to baseline.
- Don't build RESP3, sharding, or io-thread cleverness now. The phase is boring on purpose; boring here is velocity everywhere else.

---

# Chapter 19 — Phase 1: keyspace, strings, expiry — done right

**Goal**: the string type and key-management surface, semantics byte-identical to Redis, expiry per ch. 7. Retires audit findings 7, 10, 11, 13, 14, 18.

## 19.1 The store package

`redisDb{dict map[string]*Obj; expires map[string]int64}` — Go map for now, **replaced by your dict in Stage 3** behind this same API (that's why the API exists):

```
LookupRead(key)  → *Obj      // expireIfNeeded inside; nil if missing/expired; LRU touch; hit/miss stats
LookupWrite(key) → *Obj      // same + write-path expiry semantics
Set(key, obj)    // dirty++, hooks: touchWatched (stub), signalReady (stub), notify (stub)
SetExpire(key, atMs) / GetExpire / RemoveExpire
Delete(key)      // full write hooks
Size(), RandomKey(), Scan(cursor, count, match)
```

Hooks are declared now, no-ops until Phases 3–6 fill them: **the choke point is the phase's architectural deliverable.** Move `KeyspaceStat` into the db struct (fix the nil-map panics), plus `expires` count and hit/miss for `INFO`.

`expireIfNeeded` per §7.2 — including the `isReplica` parameter that currently always passes false, and the propagation hook that currently no-ops. Write it *right*, wire it *later*.

## 19.2 Command surface

Strings: `SET` (full matrix: NX XX GET EX PX EXAT PXAT KEEPTTL — mutual-exclusion errors included), the deprecated-but-everywhere legacy spellings `SETNX`/`SETEX`/`PSETEX`/`GETSET` (all four are one-liners over setGenericCommand once SET's matrix exists — that's the lesson), `GET`, `GETDEL`, `GETEX`, `APPEND` (forces raw!), `STRLEN`, `SETRANGE`/`GETRANGE`, `INCR`/`DECR`/`INCRBY`/`DECRBY` (overflow: `ERR increment or decrement would overflow`), `INCRBYFLOAT` (formatting: no trailing zeros — diff against real; and remember §9.4, it propagates as SET later), `MSET`/`MGET`/`MSETNX`.

Also `LCS` (7.0: longest common subsequence of two string keys, with `LEN`/`IDX`/`MINMATCHLEN`/`WITHMATCHLEN`) — optional, but instructive: it is plain dynamic programming at **O(N×M) time _and_ memory**, so on two 1 MB strings it allocates a terabyte-scale matrix and real Redis simply refuses past a limit. Building it teaches the §1.2 lesson from the inside — the server hands you a sharp tool, documents the complexity, and trusts you.

Keys: `DEL`, `UNLINK` (alias for now — GC is lazy-free), `EXISTS` (with repeats — `EXISTS k k k` counts 3), `TYPE`, `TOUCH`, `COPY` (with `DB`/`REPLACE`; must deep-copy the value, and *not* the TTL unless asked), `RENAME`/`RENAMENX` (as a store-level swap, fixing audit #13; TTL travels; `no such key` error), `KEYS` (your own glob: `* ? [abc] \x` — port `stringmatchlen`, ~40 lines, audit #14), `SCAN` (with MATCH/COUNT/TYPE — cursor contract §5.3; with Go maps use the keys-snapshot trick temporarily, honest cursor arrives with the dict), `RANDOMKEY`, `TTL`/`PTTL` (rounding!), `EXPIRE`/`PEXPIRE`/`EXPIREAT`/`PEXPIREAT` (+ NX XX GT LT; past→delete, audit #7), `PERSIST`, `DBSIZE`, `FLUSHDB`/`FLUSHALL`, `OBJECT ENCODING|IDLETIME|FREQ|HELP`, `SELECT` (0 only), `DEBUG OBJECT|SLEEP|SET-ACTIVE-EXPIRE`.

Server: `COMMAND` (basic: count + names), `CONFIG GET/SET` (registry-backed; glob patterns), `INFO` (server, clients, memory-approx, stats, keyspace sections — real section format), `CLIENT SETNAME|GETNAME|LIST|ID`, `HELLO` (RESP2 announce only).

## 19.3 Active expiry

Port §7.3 exactly: cron case in the engine ticker; 20-key samples from `expires`; 25% repeat rule; **time budget** = 25% of the tick interval (audit #11 dies); expired deletions go through `Delete` (full hooks — so when propagation lands in Phase 3/5, expiry propagates for free). `DEBUG SET-ACTIVE-EXPIRE 0` toggles it for tests.

## 19.4 Done-when

```bash
tests/harness/diff.sh tests/phase1_commands.txt      # ~400 lines covering every command+edge above: zero diffs
redis-cli -p 7379 SET k v EX 0                        # ERR invalid expire time in 'set' command
redis-cli -p 7379 EXPIRE k -1  → 1; EXISTS k → 0      # past-TTL deletes
redis-cli -p 7379 SET n 9223372036854775807; INCR n   # ERR increment or decrement would overflow
redis-cli -p 7379 SET s abc; APPEND s d; OBJECT ENCODING s   # raw
# SCAN never misses: script inserts 10k keys, SCANs with COUNT 10 while another client churns inserts/deletes
#   → every stable key appears ≥ once
# active expiry under budget: create 1M keys TTL 1s; measure max command latency during the die-off < 5ms
```

## 19.5 Traps

- `SET k v GET` on a wrong-type key errors *before* writing. `SET k v NX GET` combos: check real behavior, don't guess (the rules changed in 7.0 — GET is now allowed with NX).
- TTL rounding: `PTTL` exact ms; `TTL` rounds *to nearest second* — off-by-one diffs from the harness are the test working.
- `INCRBYFLOAT` output formatting (`%.17g` then trim) — the harness catches it; match it.
- SCAN's cursor is decimal-string in the protocol; `0` terminates. Your temporary map-based SCAN must still honor "cursor survives across calls while keys churn."
- Keep every error string in one `errors.go` — the harness will make you touch them repeatedly; one place beats twenty.

---

# Chapter 20 — Stage 3: the data-structure libraries

**Goal**: five packages under `core/ds/`, each standalone, tested, benchmarked, and API-shaped for Phase 2. No server changes except swapping the keyspace onto your dict at the end. Order: intset (warm-up) → dict → listpack → skiplist → quicklist.

## 20.1 `ds/intset` (~4 h)

`type IntSet struct{ enc uint8; buf []byte }` — widths 16/32/64, sorted, binary-search membership, upgrade-on-demand (§5.7). API: `Add(int64) (added bool)`, `Remove`, `Contains`, `Len`, `Get(i)`, `Random()`. Tests: property vs `map[int64]struct{}` model; upgrade at boundary values (int16 max+1 etc.); never-downgrade.

## 20.2 `ds/dict` (~10 h)

Per §5.2: two tables, power-of-two sizing, `rehashidx`, step-on-every-op + `RehashMillis(budget)` for cron; `Scan(cursor, fn)` reverse-binary; `RandomKey()` honest-random (bucket-then-chain); load-factor 1.0 grow (5.0 forced), 0.1 shrink. Hash: `maphash` seeded per-dict.
Tests: model-diff property test under random ops; **SCAN guarantee under forced rehash mid-scan** (the §5.3 done-when); rehash-completes-on-idle via budget calls; benchmark vs native map (expect ~2-3× slower — you're buying capabilities, not speed; *record the number honestly in the README*).
Then: swap `redisDb` onto it behind the Phase 1 API; re-run Phase 1's harness — **zero diffs** is the acceptance test; SCAN drops the snapshot hack.

## 20.3 `ds/listpack` (~15 h — budget it, it's fiddly)

Exact format of §5.4 (match Redis's byte format precisely — this pays off in Phase 3 when RDB dumps listpacks raw, and it makes `hexdump` comparisons against real Redis possible). API: `New`, `Insert(pos)`, `Append`, `Delete(pos)`, `Get(pos) (val []byte, isInt bool, i64 int64)`, `Len`, `Bytes`, iterators both directions, `Seek`.
Tests: round-trip property test (random op sequence vs `[]string` model); **fuzz the decoder** (corrupt bytes must error, never panic — this listpack gets loaded from RDB files later, i.e., from disk you can't trust); back-length correctness by walking backward over random content; every integer-width boundary.

## 20.4 `ds/skiplist` (~10 h)

Per §5.6: levels p=0.25 max 32, **spans**, backward pointers; score-then-member ordering (member compare = bytes). API: `Insert(score, member)`, `Delete`, `UpdateScore` (delete+reinsert unless position unchanged — Redis optimizes this; you can too), `FirstInRange/LastInRange(min, max, minEx, maxEx)`, `Rank(member’s score+name) int`, `GetByRank(n)`, `DeleteRangeByScore/Rank`.
Tests: model = sorted slice; property-test all ops; span-consistency invariant checked after every op in tests (`rank(GetByRank(i)) == i`); the p=0.25 level distribution sanity check.

## 20.5 `ds/quicklist` (~6 h)

Doubly-linked nodes wrapping your listpack; fill limit from config (count 128 or size 8KB per node); split/merge; API mirrors list commands' needs: PushHead/Tail, PopHead/Tail, `InsertBefore/After(pivot)`, `Index(n)`, `Range(from,to)`, `RemoveRange`, iterator with cursor stability across node boundaries.
Tests: model-diff vs `[]string`; node-count sanity under interleaved push/pop (no leak of empty nodes); merge threshold behavior.

## 20.6 Done-when

`go test ./core/ds/... -count=1` green; fuzzers clean for 5 min each; benchmarks recorded in `core/ds/README.md`; Phase 1 harness still zero-diff on the dict-backed store.

---

# Chapter 21 — Phase 2: the collections

**Goal**: lists, hashes, sets, sorted sets — full core command surface, correct encodings with conversions at exact thresholds. After this phase DiceMe is a *useful database*.

## 21.1 The wiring pattern (same for all four types)

Each type gets `core/cmd/<type>.go` + an object layer owning the encoding switch:

```
listTypeGet(obj) — asserts TYPE, returns an interface over {listpack | quicklist}
listTypeTryConversion(obj) — after growth: entries > list-max-listpack-size,
                             or any element > threshold bytes → convert up, once
```

Conversion = build the big structure from the small one, replace `obj.Value` + encoding, **never in place while a snapshot is live** (§9.5 — the discipline starts now even though snapshots arrive next phase; document it on every mutator).

Every write goes through store hooks (dirty, notify, signalReady — the last one matters this phase: lists feed BLPOP later).

## 21.2 Command surface (per type)

**Lists** (listpack→quicklist): LPUSH/RPUSH/LPUSHX/RPUSHX, LPOP/RPOP (+count), LLEN, LRANGE (negative indexes!), LINDEX, LSET, LINSERT BEFORE|AFTER, LREM (±count semantics), LTRIM, LMOVE/RPOPLPUSH, LPOS (RANK/COUNT/MAXLEN).
**Hashes** (listpack→dict): HSET(multi)/HSETNX, HGET/HMGET/HGETALL, HDEL, HLEN, HEXISTS, HKEYS/HVALS, HINCRBY/HINCRBYFLOAT, HSTRLEN, HRANDFIELD, HSCAN.
**Sets** (intset→listpack→dict): SADD/SREM, SISMEMBER/SMISMEMBER, SMEMBERS, SCARD, SPOP(+count — remember §9.4!), SRANDMEMBER (±count semantics differ — read the doc), SMOVE, SINTER/SUNION/SDIFF (+STORE, +SINTERCARD), SSCAN.
**Zsets** (listpack→skiplist+dict): ZADD (NX XX GT LT CH INCR — the full matrix from `zsetAdd`), ZREM, ZSCORE/ZMSCORE, ZCARD, ZINCRBY, ZRANK/ZREVRANK (WITHSCORE), ZRANGE (REV, BYSCORE, BYLEX, LIMIT — the unified 6.2 form + legacy ZRANGEBYSCORE etc.), ZRANGESTORE, ZCOUNT, ZLEXCOUNT, ZPOPMIN/ZPOPMAX, ZRANDMEMBER, ZREMRANGEBYRANK/SCORE/LEX, ZSCAN. (ZUNION/ZINTER/ZDIFF +STORE if you have appetite — the weights/aggregate options are fiddly but mechanical.)
**Multi-key pops** (7.0 forms, once lists+zsets exist): LMPOP/ZMPOP — first non-empty of N keys; their blocking twins land in Phase 6.
**Optional now, menu later**: `SORT`/`SORT_RO` (`sort.c:sortCommandGeneric` — BY/GET pattern dereferencing, ALPHA, LIMIT; the closest thing Redis has to a join, and a good rainy-day build) and hash **field TTLs** (7.4's HEXPIRE family — Phase 8 menu; note now that your hash object layer will need per-field metadata someday, but don't design for it yet).

Score parsing: `+inf`/`-inf`/`(3.5` exclusive ranges; lex ranges `[a (b - +`. Diff-harness these hard — range-boundary bugs are the phase's signature failure.

`SizeOf()` on every representation lands **now** (§4.3), even though eviction reads it two phases later.

## 21.3 Done-when

```bash
tests/harness/diff.sh tests/phase2_commands.txt        # the big one: ~1000 lines, all four types, zero diffs
# encoding flips at exact thresholds:
redis-cli -p 7379 CONFIG SET hash-max-listpack-entries 3
#   HSET 3 fields → listpack ; 4th field → hashtable ; never back
# 1M-member zset: ZADD in pipeline; ZRANGEBYSCORE latency sane; ZRANK O(log n) sane (time it)
# storm/mixed: interleaved type ops, 10 min, RSS stable, zero errors
```

## 21.4 Traps

- LREM/LINSERT/LPOS on quicklist cross node boundaries — the iterator tests from 20.5 earn their keep.
- Empty-collection rule: **a collection that becomes empty is deleted** (`HDEL` last field → key gone, `EXISTS` → 0). Every pop/rem command needs this; forgetting it diverges DBSIZE/EXISTS/TYPE everywhere (the harness will scream).
- SINTERCARD/SRANDMEMBER negative-count semantics: read the command docs *while writing the tests, before the code*.
- ZADD GT/LT + NX combinations are errors; the matrix in `zsetAdd` is the truth table — port it, don't re-derive it.
- Blocked on a range-boundary diff for an hour → you're inclusive/exclusive-swapped on one end; check `(` handling first.

---

# Chapter 22 — Phase 3: persistence

**Goal**: RDB save/load, AOF with all three fsync policies, background rewrite, correct boot loading, and the §9.6 crash matrix enforced by tests. Retires audit finding 4. Read ch. 29 first.

## 22.1 Order of work

1. `SAVE` (blocking, simple serializer) + `rdbLoad` at boot — the format, debugged in peace.
2. The snapshot mechanism (§22.3) → `BGSAVE` + save points.
3. AOF feed/flush/fsync + load — durability.
4. `BGREWRITEAOF` on the snapshot mechanism — compaction.
5. Crash matrix under storm.

## 22.2 RDB build notes

Format from §9.2. Write into a `bufio.Writer` wrapped in a CRC64-computing writer (implement the Jones polynomial per `crc64.c`, or vendor the table — it's a constant). Value serializers per encoding, packed-encodings-dump-raw where your listpack is byte-compatible (it is, if 20.3 matched the format — reward collected). Loader: strict, but tolerant of *newer* value types by erroring with names, and validating listpacks via the fuzz-hardened decoder before trusting them (§20.3's "disk you can't trust").

Boot order per §15.2: load before serving; big files → `-LOADING Redis is loading the dataset in memory` for early connections (test it with a deliberate slow-load debug flag).

Save points: `save 900 1` config parsing; cron checks dirty counter + elapsed; `SHUTDOWN` saves if any save point configured (and `SHUTDOWN NOSAVE` doesn't).

**Stretch with high payoff**: make your RDB *actually loadable by real `redis-server`* (and vice versa). It forces byte-exactness everywhere and turns the oracle up to maximum. Budget +6 h; worth it.

## 22.3 The snapshot mechanism (§9.5 option B, specified)

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

The test you wrote before implementing (§9.5): snapshot live → mutate every type → serializer sees pre-mutation values. Plus: memory during snapshot bounded (RSS < 2× dataset for a 100%-write-touch workload — the honest COW-equivalent worst case).

`ShallowCopy` pause: measure with 1M keys, record it. If >200 ms, incrementalize per-bucket (the dict makes that possible — another 20.2 reward) — but only if measured.

## 22.4 AOF build notes

Per §9.3–9.4. The engine owns `aofBuf`; the propagation hook (declared in Phase 1, filled now) appends the **effect** form. Flush in the engine's per-iteration tail (your beforeSleep point — after draining a batch of requests, before selecting again). fsync: `always` inline *before* replies release (the honesty rule — restructure reply-release to after-flush for this mode); `everysec` on a dedicated goroutine (your bio thread), with the **stall rule**: if the previous fsync is still running, the *flush* blocks — bounded memory beats bounded latency here, same trade Redis makes.

Rewrites via §22.3 snapshots: serialize minimal commands (or RDB-preamble: emit your RDB bytes, then `*`-commands after — do commands first, preamble as the final step). During rewrite, engine appends live writes to both the real AOF and a rewrite-tail buffer; on child-done: temp file + tail splice + `rename` + fsync dir (§29.2), swap fds, bio-close the old one (§14.1's "why close is on the list" — now you do it too).

Loader: fake client from Phase 0 pays off — read RESP arrays, dispatch through the normal table with propagation *disabled* (a replay must not re-feed the AOF) — add the `CLIENT_REPLAY`-style flag to dispatch now; replication reuses it in Phase 5. Torn tail: `aof-load-truncated yes` behavior + test.

`WAITAOF` — skip; `appendfsync` switching at runtime via CONFIG SET — support, it's a config-registry validator away.

## 22.5 Done-when

```bash
# the crash matrix, mechanized (each row = a script):
tests/phase3_crash.sh rdb-only      # kill -9 under storm; restart; data == last save point
tests/phase3_crash.sh aof-everysec  # loss window ≤ 1s of acks (storm records acked writes w/ timestamps)
tests/phase3_crash.sh aof-always    # ZERO acked writes lost (the hard one — proves reply-after-fsync)
tests/phase3_crash.sh rewrite-race  # kill -9 DURING BGREWRITEAOF x20 → always boots, never corrupt
redis-cli -p 7379 BGSAVE            # under storm: no client sees >200ms latency (measure!)
# snapshot isolation: the §22.3 test, all five type rows
# stretch: dump.rdb loads in real redis-server && real dump.rdb loads in dice
```

## 22.6 Traps

- fsync ≠ write. `File.Sync()` is the fsync. The matrix rows fail without it and pass with it — run the always-row against a version without Sync once, watch it lose data, keep the log as a trophy.
- Rename without fsyncing the *directory* can lose the rename itself (§29.2).
- The everysec ack-recording storm client must record what the *server acked*, not what it sent — the difference is the whole measurement.
- AOF replay must run with active-expiry off and clock-independent (that's why §9.4 rewrote to PEXPIREAT). A key that expired between crash and restart: replay SETs it, replay PEXPIREATs it into the past, first lookup deletes it. Test that sequence.
- Go's `os.File.Write` is already unbuffered — your `bufio` layer is where "written but not flushed" hides. Audit every path for flush-before-fsync.

---

# Chapter 23 — Phase 4: maxmemory and eviction

**Goal**: byte-budget memory accounting, all eight policies, the real pool. Retires audit findings 3, 8, 12.

## 23.1 Accounting

`SizeOf()` (laid in Phases 1–2) rolls into an engine counter maintained at the choke point: Set/Delete/mutations adjust by delta (mutators return size deltas — retrofit is mechanical since mutation already funnels through type-layer functions). Include: obj headers (a constant you calibrate once against `runtime.ReadMemStats` with 1M uniform keys — document the calibration in a comment), key strings, expires entries, and (later) replication backlog + client output buffers (Redis counts those too — add in Phase 5; it matters: a 256 MB replica buffer *is* memory).
`INFO memory`: `used_memory` (your counter), `used_memory_rss` (`runtime.ReadMemStats.Sys` — labeled approximate), `maxmemory`, `maxmemory_policy`, `evicted_keys`.

Retire `KeysLimit` (audit #8); `CONFIG SET maxmemory 100mb` with the size-suffix parser.

## 23.2 performEvictions

Port Session 8's loop: on write-command dispatch when over budget → compute overage → loop {sample per policy (16 from your dict's honest `RandomKey`, or the expires dict for volatile-*) → pool-insert → evict pool's best → full delete hooks (propagates DEL! §8.5) → recount} until under or nothing evictable → then `noeviction`-style `-OOM` for the triggering write if still over. Reads never blocked. `CmdDenyOOM` flag from Phase 0's table decides who's a "write" here — reward collected.

## 23.3 The pool, correctly (audit #3's funeral)

Rewrite `Push` per `evictionPoolPopulate`: pool ascending by idle; binary-search rank; full+worst → skip; else insert-at-rank (memmove — it's 16 entries, a copy is fine), evicting the **least idle** end; keyset bookkeeping exact. Property tests: (a) pool always sorted, (b) keyset ≡ pool members, (c) after N random pushes, pool ≡ top-16-by-idle of all pushed. Then the LFU trio (`LFULogIncr` probabilistic bump, `LFUDecrAndReturn` decay, minutes clock) — same pool ranked by counter.

## 23.4 Done-when

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

## 23.5 Traps

- Accounting drift: every mutation path that forgets its delta shows up as slow leak vs maxmemory. Add a `DEBUG RECOUNT-MEMORY` that walks and re-sums, assert drift == 0 at the end of every storm run — cheap invariant, catches everything.
- Don't evict *while executing* the triggering command's lookup phase — evict at the dispatch gate (or you invalidate objects the command already holds — the C code evicts before `call()` for this reason; your audit-#13 Rename-eviction reentry was this bug's cousin).
- 24-bit clock wraparound: your `getIdleTime` is right; keep its test anyway — the person who "simplifies" it later is also you.

---

# Chapter 24 — Phase 5: replication — the wall

**Goal**: master + replica roles, PSYNC full and partial, backlog, correct propagation, ACK/WAIT, surviving link chaos. Read ch. 10 and Session 11 immediately before. This phase has the highest bug-hours-per-line in the project; ch. 30's discipline (log every state transition, merge logs) is not optional here.

## 24.1 Order of work

1. **Propagation stream** (even with zero replicas): the engine builds the canonical effect stream — §9.4 shapes, offset counter, ring-buffer backlog. Unit-test the ring hard (wrap, overwrite, read-from-offset, offset-out-of-range).
2. **REPLICAOF + full sync**: replica-side state machine (connect → handshake → receive RDB via §22.3-powered BGSAVE on master → flush → load → stream). Master-side: `syncCommand`, per-replica during-save queue.
3. **Steady state**: feed replicas from propagation; replica applies via fake-client (+`CLIENT_MASTER` flag: no reply, no re-propagation... except onward to *its* sub-replicas — verbatim forwarding, §10.1); ACK every second; `INFO replication` both sides.
4. **Partial resync**: replid/replid2 + backlog window gates → `+CONTINUE`; the offset bookkeeping *will* be wrong the first four times; the byte-level log-merge (ch. 30) is how you find it.
5. **WAIT**, `ROLE` (returns `master`/`slave` plus offsets and the peer list — the pre-INFO way clients discover topology, and three lines once the state exists), `min-replicas-to-write`, replica-read-only (`-READONLY You can't write against a read only replica.`), expiry rule enforcement (§7.4 — the replica branch of expireIfNeeded finally goes live), replica eviction off.
6. **Chaos**: the drill matrix below.

## 24.2 Design notes

- The engine owns: backlog, replica registry, offsets. Each replica connection = a client with `CLIENT_SLAVE` flag whose writer goroutine drains the feed; slow replica → output-limit kill (§10.4) — implement the limit *now*, the full-resync-loop chaos drill needs it.
- Replica side is a *client of the master*: one goroutine runs the §10.3 state machine and then becomes a reader pumping the master's stream into the engine as fake-client commands. Reconnect-with-backoff on any error, re-entering at PSYNC with saved (replid, offset).
- Offset discipline: the offset counts **bytes of the canonical stream**, master-side; the replica counts bytes *as received and applied*. PINGs from master ride the stream and advance offsets (that's their point). Never count bytes of anything else.
- RDB transfer: length-prefixed from your snapshot file. Diskless (`$EOF:<delim>`) — stretch.
- Persist nothing about replication state except (per Redis) an RDB-carried replid/offset — actually simplest correct: on clean shutdown as replica, forget; always PSYNC afresh with what memory holds. (Real Redis persists more for restart-partial-resync; stretch.)

## 24.3 Done-when

```bash
# 2 terminals: dice-master :7379, dice-replica :7380 (REPLICAOF localhost 7379)
redis-cli -p 7379 SET k v ; redis-cli -p 7380 GET k          # v, within 10ms (measure the lag)
redis-cli -p 7380 SET x 1                                     # -READONLY …
redis-benchmark -p 7379 -n 100000 -P 16 -t set ; # replica DBSIZE converges to master's
# partial resync: tcpkill/close the replica's conn mid-benchmark (a storm flag drops it);
#   master log: "+CONTINUE"; NO RDB transfer (log proves it); datasets converge (DEBUG DIGEST-equivalent — build one: CRC of sorted key/value dump)
# backlog overflow: stop replica 60s under heavy writes with tiny repl-backlog-size → reconnect does FULL resync; converges
# expiry: SET k v PX 900 on master; replica GET k at t+950ms → nil BUT replica DBSIZE still counts it until master's DEL arrives (test the asymmetry!)
# WAIT: WAIT 1 100 returns 1 with live replica; kill replica → WAIT 1 100 returns 0 after ~100ms
# chained: replica-of-replica :7381 converges; digests all equal
# the full-resync loop, reproduced: tiny replica output limit + slow replica (storm --slow-reader) → observe kill/reconnect/fullsync cycle in logs; then fix by raising limit; document in NOTES.md
# kill -9 master; restart (AOF); replicas full-resync (new replid) and converge
```

## 24.4 Traps

- The #1 bug: feeding replicas the client's *verbatim* command instead of the canonical effect (SPOP, INCRBYFLOAT, EX-relative). Your Phase 3 propagation hook already canonicalizes — replication must tap the *same* stream, one producer. If you build a second path "just for replication," you will diverge; don't.
- The #2 bug: off-by-N offsets from counting reply bytes, or from the handshake's RDB bytes (the RDB is NOT part of the stream; offset starts after). Symptom: +CONTINUE followed by protocol garbage on the replica — it's parsing mid-command.
- Replica applying a command that fails (WRONGTYPE from divergence): real Redis logs and *panics* on inconsistency in strict mode; at minimum log loudly and count — silence buries the real bug.
- BLPOP-served-by-replicated-LPOP arrives (§16.3): the replica has no blocked client; the stream's RPUSH+LPOP must leave its list state identical. If your list is "empty ⇒ key deleted" (Phase 2 trap) on both sides, it works; test proves it.
- Your everysec-fsync bio goroutine + replica feed both hang off the engine tail — starving one under load is easy; the lag measurement in done-when is the canary.

---

# Chapter 25 — Phase 6: transactions, blocking, pub/sub

**Goal**: MULTI/EXEC/DISCARD/WATCH/UNWATCH, BLPOP/BRPOP/BLMOVE/WAIT-style blocking, SUBSCRIBE family, keyspace notifications. Ch. 13 is the spec; the choke-point hooks stubbed since Phase 1 all go live. This phase is where the engine-goroutine choice (§14.4) pays out — estimate says 25 h *because* of it.

## 25.1 Build notes

- **MULTI**: flags + queue on the Client (fields exist since Phase 0); dispatch gate: in-MULTI × not-{EXEC,DISCARD,MULTI,WATCH,RESET} → validate-and-queue (+QUEUED / flag DIRTY_EXEC on validation error). EXEC: gates (DIRTY_EXEC → EXECABORT; DIRTY_CAS → null), then run the queue inline as one engine message; wrap propagation in MULTI/EXEC (via the effect mechanism — a batch effect).
- **WATCH**: `watched_keys` dict on the db + list on client; `touchWatchedKey` in the Phase 1 hook slot; fires on every effective write **including expiry and eviction deletes** (they already funnel — reward). UNWATCH + auto-unwatch after EXEC. Cross-db WATCH if you did SELECT — or reject WATCH on db>0, documented.
- **Blocking**: registry per §13.3 (`blocking_keys`, `ready_keys`); the two-phase wake (signal at choke point → serve after current message completes — in engine terms: collect ready keys during dispatch, process them at message tail, before AOF flush hooks); FIFO per key; timeout wheel = simplest: check blocked clients in cron tick (100 ms granularity is fine; Redis is similar). Propagate the effective pop (§16.3). BLPOP/BRPOP/BLMOVE + WAIT (blocks on ack offsets — replication integration test in one command); then BLMPOP/BZPOPMIN/BZPOPMAX/BZMPOP — same registry, different pop function (the zset ones signal-ready from ZADD, proving the choke point generalizes).
- **Pub/sub**: two registries (channels, patterns — pattern match with your Phase 1 glob), publish = immediate buffer pushes + count reply; RESP2 subscriber-mode restriction; PUBSUB CHANNELS/NUMSUB/NUMPAT introspection; propagate PUBLISH to replicas (subscribers on replicas hear it — test).
- **Notifications**: `notify-keyspace-events` config parsing (the flag-letters), `notifyKeyspaceEvent` in the hook slot, events per command class as documented. ~30 lines *because* the architecture (told you in §13.5).

## 25.2 Done-when

```bash
tests/harness/diff.sh tests/phase6_multi.txt      # queue/abort/dirty matrices vs real redis
# WATCH races: two raw-conn Go clients, scripted interleave → exactly one EXEC null; loop ×1000 deterministic
# WATCH + expiry: WATCH k (TTL 100ms) → sleep → EXEC → null
# BLPOP: A blocks; B pushes; A wakes with value; replica saw RPUSH,LPOP; FIFO with 3 blockers; timeout returns null-array at ±100ms
# BLPOP inside MULTI does NOT block (returns immediately/null — real semantics: it degrades; diff it!)
# pub/sub: 100 subscribers × 10k messages, ordering per subscriber preserved; pattern overlap gets both deliveries
# notifications: __keyevent@0__:expired observed for an active-cycle expiry (watch your own machinery on the wire!)
```

## 25.3 Traps

- EXEC's queue runs even with runtime errors mid-batch (§13.1); only *queue-time* validation aborts. The diff file must include both matrices.
- Wake-before-propagation-complete ordering bug: a blocked client served *within* B's MULTI batch observes a half-applied batch. Your two-phase design prevents it; the test is the MULTI{RPUSH,LPOP} case from §13.3.
- Client disconnect while blocked / while in MULTI / while subscribed — the Phase 0 cleanup function grows its last three tentacles; leak-test each.
- WAIT inside MULTI: returns immediately with current acks (can't block) — obscure, diff it anyway.

---

# Chapter 26 — Phase 7: cluster — the second wall

**Goal**: slots, redirects, live resharding, gossip membership, PFAIL→FAIL conviction, replica failover with epochs. Ch. 12 + Session 14 are the spec. Wire format is *yours* (JSON or your own binary — logic over bytes); everything else is the real protocol.

## 26.1 Order of work

1. **Slots, static**: `cluster-enabled yes`; CRC16 (port the table) + hash-tag extraction; `getNodeByQuery` port: single-slot enforcement (`-CROSSSLOT`), MOVED for foreign slots; `CLUSTER KEYSLOT/SHARDS/SLOTS/MYID/INFO`; static config bootstrap (`CLUSTER SETSLOT`-style assignment via a script) → 3 masters, `redis-cli -c` works. *Cluster mode restricts to db 0.*
2. **The bus**: second listener (port+10000); node registry (id, addr, flags, slots bitmap, epochs, last-ping/pong); MEET → handshake → gossip: periodic PING to random/oldest peers, each carrying N random gossip entries (your ConsulMe SWIM muscle, directly). New nodes converge to full membership + slot map with zero central config.
3. **Conviction**: PFAIL by timeout → gossip carries others' PFAILs → majority of masters → FAIL broadcast. (Your SWIM toy's suspicion machinery, renamed.)
4. **Resharding**: IMPORTING/MIGRATING states; ASK/ASKING; `CLUSTER GETKEYSINSLOT`; MIGRATE as DUMP→RESTORE→DEL (your RDB value-serializer from Phase 3 *is* DUMP's payload format — reward); `SETSLOT NODE` with epoch bump; a `dice-cli-reshard` script driving it live under storm.
5. **Failover**: cluster replicas = Phase 5 replication + bus membership; election per §12.5 (rank delay, masters-only votes, epoch gates, majority); promotion claims slots with new configEpoch; UPDATE messages fix stale nodes; epoch-collision tiebreak.
6. **Chaos drills.**

## 26.2 Done-when

```bash
tests/phase7_cluster.sh up          # 6 nodes, 3+3, slots 0-5460/5461-10922/10923-16383
redis-cli -c -p 7379 SET user:{42}:a 1 && GET via any node   # transparent redirects
MSET k{t}1 v k{t}2 v                # ok;  MSET a b c d → -CROSSSLOT
tests/phase7_reshard.sh 100         # move 100 slots live under storm: zero client errors (ASK path exercised — count ASKs in logs > 0)
kill -9 <master2>                   # ≤ node-timeout+2s later: its replica promoted, cluster_state:ok, writes flow; old master returns → demotes to replica (log shows epoch reasoning)
# partition drill (iptables/pf or a --drop-peer test hook): minority master's clients get errors after timeout (cluster-require-full-coverage both settings tested); heal → converge; count lost writes; write the number in NOTES.md next to §10.6
# gossip convergence: node 7 MEETs node 1 only; knows all slots+nodes < 5s
```

## 26.3 Traps

- Slot bitmap in gossip must be the *sender's claimed* slots; receivers believe it only with epoch ≥ what they know for those slots — skipping the epoch check makes resharding + failover corrupt each other (the UPDATE message exists for the losing side).
- ASK without ASKING must be refused by the importer (else clients that skip ASKING see keys "arrive early" — breaks the one-owner invariant).
- MIGRATE's DEL on source must be atomic with the transfer *from the client's perspective* — engine model gives it (one message does serialize+send+await+delete... careful: awaiting a network reply inside the engine blocks everything — the one place you must **not** hold the engine: mark keys "migrating-busy," do the transfer async, fail racing writes with `-TRYAGAIN` — which is exactly what real Redis returns; Session 14 shows it).
- Election without the rank delay works fine in tests and elects the stalest replica in production-shaped chaos; keep the delay, verify by making offsets deliberately unequal in the drill.
- 6 processes' logs are unreadable separately; the ch. 30 merge script is mandatory equipment here.

---

# Chapter 27 — Phase 8: the stretch menu

Pick à la carte; each is independent. Rough effort attached.

| Feature | Effort | What it teaches / notes |
|---|---|---|
| **Sentinel** (§ch. 11) | 25 h | Separate `dice-sentinel` binary: monitor, SDOWN/ODOWN quorum, hello-channel discovery, leader vote, failover orchestration + the 50-line Go client. Best pick if you skipped Cluster. |
| **Geospatial** (GEOADD/GEOPOS/GEODIST/GEOHASH/GEOSEARCH/GEOSEARCHSTORE + legacy GEORADIUS forms) | 12 h | §5.9: Morton-interleaved 52-bit geohash as zset score; 9-cell neighbor search → range scans + haversine filter. Zero new storage — the best "map a query onto an existing index" lesson in the codebase. `geo.c`, `geohash.c`, `geohash_helper.c`. |
| **Streams** (XADD/XRANGE/XREVRANGE/XLEN/XREAD[+BLOCK]/XTRIM/XDEL/XSETID + groups: XGROUP/XREADGROUP/XACK/XPENDING/XCLAIM/XAUTOCLAIM/XINFO) | 30 h | Your `ds/rax` build + consumer-group semantics (PEL, delivery counts, MAXLEN~ approximate trimming). The durable pub/sub. |
| **Hash field TTLs** (7.4: HEXPIRE/HPEXPIRE/HEXPIREAT/HPEXPIREAT/HTTL/HPTTL/HPERSIST/HGETEX/HGETDEL) | 10 h | Per-*field* expiry inside one value — forces per-field metadata (`t_hash.c` HFE sections, `listpackex` encoding) and a second expiry index; the same ch. 7 contract one level down. |
| **SORT / SORT_RO** (BY/GET pattern deref, ALPHA, LIMIT, STORE) | 6 h | `sort.c:sortCommandGeneric`; the closest thing Redis has to a join; pattern-dereference (`weight_*`, `GET obj_*->field`) is a lovely gnarly parser exercise. |
| **Lua scripting** (EVAL/EVALSHA/SCRIPT) + **Functions** (FUNCTION LOAD/LIST/CALL — the 7.0 librarified successor) via gopher-lua | 15 h | Effects propagation (have it), script cache, `redis.call` bridge, busy-script + kill. |
| **RESP3** (HELLO 3, typed replies, push frames) | 10 h | The §3.7 serializer swap; big-payoff-per-hour; unlocks client tracking. |
| **Client tracking / CSC** (CLIENT TRACKING, invalidation push) | 10 h | Needs RESP3. The keyspace choke point fires invalidations — 30 lines, again. |
| **ACLs** (users, command/key patterns, AUTH) | 12 h | A dispatch gate + config surface. Mechanical. |
| **SLOWLOG + LATENCY** | 6 h | Ring buffer of slow calls (time each eval — you already do for stats); LATENCY HISTORY/RESET; pairs with monitoring below. |
| **Prometheus exporter** for your `monitoring/` dir | 6 h | `/metrics` HTTP listener translating INFO fields; wire the existing Grafana dashboards to DiceMe; the storm + dashboard demo is the best show-someone artifact in the repo. |
| **Bitmaps** (SETBIT/GETBIT/BITCOUNT/BITPOS/BITOP + **BITFIELD** with u/i types, OVERFLOW WRAP\|SAT\|FAIL) | 10 h | Bit-twiddling on raw-encoded strings (`bitops.c:bitfieldGeneric`); BITFIELD is a miniature typed VM over a byte array. |
| **HyperLogLog** (PFADD/PFCOUNT/PFMERGE) | 15 h | Flajolet's paper + Redis's dense/sparse register encodings (`hyperloglog.c`); cardinality in 12 KB — the most magical 300 lines you'll write. |
| **Replication extras** (WAITAOF; `FAILOVER` — coordinated, loss-less manual failover) | 8 h | FAILOVER choreography (pause writes → wait replica catch-up → swap roles) is WAIT + PAUSE + PSYNC composed; a mini-Sentinel in one command. |
| **Sharded pub/sub** (SSUBSCRIBE/SPUBLISH/SUNSUBSCRIBE) | 4 h | §13.4's cluster fix; needs Phase 7's bus. |
| **Misc small commands** (TIME, SUBSTR, MOVE, SWAPDB, LOLWUT, OBJECT REFCOUNT, MEMORY STATS\|DOCTOR, CLIENT UNPAUSE\|NO-TOUCH) | 4 h | Afternoon fillers; TIME and SUBSTR are ten-liners — do them with morning coffee. |
| **io-threads → real parallel-parsing benchmark** | 4 h | Measure your free io-threads claim (§14.3); README material. |
| **Active defrag analog / memory doctor** | — | Read `defrag.c`; write the "why Go can't/needn't" note instead. Cheapest deep insight per hour in the list. |

---

# Chapter 28 — Production-grade concerns

Do these during Phase 8, but *read* now — several change earlier designs cheaply.

## 28.1 Observability

- **INFO complete**: all sections real Redis emits (server, clients, memory, persistence, stats, replication, cpu, keyspace) with the fields your subsystems already track. `INFO everything` is your ops UI; treat gaps as bugs.
- **MONITOR**: flag on client; choke point already sees every command — stream them (`+<ts> [db addr] "SET" "k" …`). 20 lines; unreasonably useful for debugging *your own* server. (Warn: real MONITOR halves throughput; yours will too; that's authentic.)
- **Metrics**: instantaneous ops/sec (rolling window), hit/miss ratio, expired/evicted counters, connected clients/replicas, repl offset lag, AOF buffer size, fork/snapshot pause ms. Exporter per Phase 8 menu.
- **Logs**: structured (`slog`), one line per state *transition* (ch. 30), loglevel config.

## 28.2 Limits — every one is a production incident you're pre-empting

- `maxclients` (fd exhaustion) · `proto-max-bulk-len` (memory bomb per §3.3) · `client-output-buffer-limit` per class {normal, replica, pubsub}: hard bytes / soft bytes+seconds → kill (§10.4's full-resync loop, §13.4's slow subscriber — both die here) · input-buffer cap (a client that sends MULTI and queues 10M commands) · `maxmemory-clients` (7.0 **client eviction**: total client-buffer memory budget; over it, evict the biggest-buffer clients — the aggregate cousin of per-client limits) · `timeout` (idle client reaping, cron) · `tcp-keepalive`.
- Test each limit with a hostile storm mode (`storm --slow-reader --huge-bulk --queue-bomb`). "Server survives hostile clients with bounded memory" is the phase's demo.

## 28.3 Config, done properly

Full registry: every config has {name, type, default, validator, dynamic?, apply-hook}. `CONFIG GET pattern`, `CONFIG SET` (dynamic ones), `CONFIG REWRITE` (write current state back to file — preserve comments? Redis appends a generated section; do that). Startup: file → flags → env, documented precedence. `CONFIG RESETSTAT`.

## 28.4 Protections & security basics

`protected-mode` (refuse non-loopback when no password) · `requirepass`/AUTH (+ AUTH gate in dispatch — you built the slot for it in Phase 0) · `rename-command`-style disabling (config to remove FLUSHALL/DEBUG from the table) · never log values (audit your logs — key names ok, values no) · CLIENT KILL/LIST/PAUSE (PAUSE is a fun one under the engine model: just stop dequeuing — and a WAIT/failover primitive in real Redis).

## 28.5 Benchmark honestly

`make bench` table in README: dice vs real redis, same box, same `redis-benchmark` flags: PING/SET/GET/INCR/LPUSH/ZADD ×{-P 1, -P 16}. Expect: within 2-4× of C Redis single-instance; *understand* every gap you can't close (Go GC, channel hop per command ≈ 1-2 µs — measure a no-op command to isolate it, cgo-free syscalls…). A README that says "60% of C Redis and here is exactly where the other 40% goes" is worth more than the missing 40%.

## 28.6 Chaos, continuous

`storm` grows modes: mixed-type workload · TTL-heavy · pipeline bursts · slow-reader · disconnect-storm · plus a `chaos.sh` that loops: random {kill -9 a node, drop a link, fill memory, BGSAVE spam} against a running topology while a verifier client asserts invariants (acked writes durable per policy, replica convergence, cluster availability). Leave it running overnight before calling any phase done. Every failure it finds goes in Appendix F with a test.

---

# Chapter 29 — Crash safety: the filesystem contract

Read before Phase 3. Short and load-bearing.

## 29.1 What fsync buys, exactly

`write()` → page cache (RAM). Power loss eats it. `fsync(fd)` → blocks until the device confirms durable storage of that file's dirty pages (modulo lying consumer drives — note and move on). Order matters: data-fsync **before** any metadata/rename that makes readers trust the data. Cost: ~1 ms SSD (the §1.1 number shaping all of §9.3).

## 29.2 Atomic replacement, the full liturgy

```
write temp file (same directory!)  →  fsync(temp)  →  rename(temp, final)  →  fsync(directory)
```

- rename is atomic *in namespace*: readers see old or new, never mixed — but only if the new bytes were fsynced first (else: new name, garbage content after crash).
- fsync the **directory** or the rename itself may not survive (`open(dir, O_RDONLY)` + `Sync()` in Go).
- Same filesystem required (rename doesn't cross mounts) — hence temp-in-same-dir, not `/tmp` (also why your `os.CreateTemp(dir, …)` takes the dir argument).
- This liturgy is RDB save, AOF rewrite swap, and cluster/nodes.conf save. Write it once (`persist/atomicfile.go`), test it with the kill-loop, reuse thrice.

## 29.3 Torn writes & checksums

A crash can persist a *prefix* of your last write (and rarely, with some filesystems/devices, interleavings — assume prefix for AOF-with-fsync discipline). AOF: tail may be half a command → truncate-to-last-valid-command loader (§9.3). RDB: half a file with valid header → the trailing CRC64 catches it (that's *why* it's trailing) → refuse to load, fall back per config. Test rig: a proxy-writer that duplicates every AOF write to N copies truncated at random byte offsets; loader must handle all N.

## 29.4 What to test (already in Phase 3's done-when — this is the why)

kill -9 loops at randomized points; power-loss simulation = SIGKILL is honest enough for process-level (device-level lies are out of scope, note them); the always-policy zero-loss proof; the rewrite-race loop; the torn-tail rig.

---

# Chapter 30 — Debugging a database

Read before Stage 7; re-read when stuck there. Same discipline as ConsulMe ch. 27, adapted.

## 30.1 Observe, don't reason

Single-process bugs yield to reasoning. Replication/cluster bugs are *interleavings* — you will not deduce them from source; you will see them in logs or not at all. Instrument first, hypothesize second.

## 30.2 Log every state transition, and only transitions

`REPL_STATE: CONNECTING→RECEIVE_PONG`, `PSYNC sent replid=… offset=…`, `+CONTINUE granted (backlog span …)`, `FAIL conviction: node … (7/10 masters)`, `election: epoch=42 rank=1 delay=1173ms`, `snapshot: shallow-copy 1.2M keys 87ms`. Fields: timestamp(µs), node-id, subsystem, from→to, cause. Steady-state spam (every SET) belongs in MONITOR, not logs.

## 30.3 Merge logs across nodes

`sort -m` by timestamp with node-id prefixes (one machine → one clock → no ConsulMe-style skew problem; enjoy it). `scripts/mergelogs.sh *.log | less` is the cluster-phase cockpit. The bug is always visible in the merged view at the first line where two nodes' beliefs diverge — train yourself to find that line.

## 30.4 Determinism levers

- Engine model = one interleaving source (the channel). A `--record` flag that logs every dequeued command with a sequence number, and a `--replay file` that feeds them back = deterministic reproduction of any single-node bug, *including* cron timing if you log tick boundaries. Build in Phase 5's despair; ~2 h.
- Seeded randomness everywhere (`--seed`): eviction sampling, skiplist levels, election jitter. One flag, whole-run determinism.
- The AOF is already a replay log — `dice --replay-aof file` onto a debug build is time-travel.

## 30.5 Invariant assertions

`DEBUG CHECK` (build it): dict count == keyspace stat; expires ⊆ keyspace; skiplist↔dict member parity per zset; span-sum == length; memory recount == counter; replica: applied-offset == received-offset. Run in every test teardown + after chaos. A failed invariant near the cause beats a wrong answer far from it.

## 30.6 The tools you already have (use on DiceMe)

`redis-cli -p 7379 --latency` / `--latency-hist` · MONITOR (yours) · `pprof` (goroutine leaks after churn = your #1 Go-specific bug class; heap for the accounting drift) · `go test -race` on everything, always (the engine model makes races *rare*, not impossible — the snapshot handoff and the writer goroutines are where they hide) · `nc` + a text file of RESP for reproducing exact byte sequences · `hexdump -C dump.rdb` next to real Redis's.

## 30.7 The five bugs you'll actually hit

1. **Args aliasing under pipelining** (Phase 0): garbled keys under -P 16 only. Copy on channel-cross.
2. **Missing propagation on a delete path** (Phase 3/5): replica diverges by one key, hours later. The digest-compare in the drill matrix exists for this; grep all `Delete` callers.
3. **Offset drift** (Phase 5): +CONTINUE then parse garbage. Count only canonical-stream bytes; log offsets at every feed.
4. **Snapshot isolation leak** (Phase 3): one mutator skips copy-on-write; RDB contains a value from the future. The §22.3 per-type test matrix; add types to it as they're added.
5. **Wake-order violation** (Phase 6): BLPOP client observes a half-applied MULTI. Two-phase ready-keys; the §13.3 ordering test.

## 30.8 The discipline

One hypothesis at a time, written down before testing (NOTES.md). Every fixed bug gets: the invariant that would have caught it (add to DEBUG CHECK), the log line that would have shown it (add), and an Appendix-F entry (write). Never fix a distributed bug you can't reproduce — the fix is noise until the repro exists (the §30.4 levers exist to make "can't reproduce" rare).

---
---

# PART VI — APPENDICES

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
| **ziplist cascade update** | The O(N²) re-encoding chain in listpack's predecessor; the reason listpack exists. |

---

# Appendix B — Command surface per phase

The contract for "done": every command listed, semantics diff-clean vs real Redis (modulo the documented nondeterminism allowlist).

### Phase 0
`PING ECHO QUIT` · dispatch errors (unknown cmd, arity) · inline commands · `DEBUG SLEEP` · carried-over Phase-1 commands keep working

### Phase 1
Strings: `SET(NX|XX|GET|EX|PX|EXAT|PXAT|KEEPTTL) SETNX SETEX PSETEX GETSET GET GETDEL GETEX APPEND STRLEN SETRANGE GETRANGE INCR DECR INCRBY DECRBY INCRBYFLOAT MSET MSETNX MGET`
Keys: `DEL UNLINK EXISTS TYPE TOUCH COPY RENAME RENAMENX KEYS SCAN(MATCH|COUNT|TYPE) RANDOMKEY TTL PTTL EXPIRE PEXPIRE EXPIREAT PEXPIREAT(NX|XX|GT|LT) PERSIST EXPIRETIME PEXPIRETIME DBSIZE FLUSHDB FLUSHALL SELECT`
Server: `COMMAND(COUNT) CONFIG(GET|SET|RESETSTAT) INFO TIME CLIENT(ID|GETNAME|SETNAME|LIST) OBJECT(ENCODING|IDLETIME|FREQ|HELP) DEBUG(OBJECT|SLEEP|SET-ACTIVE-EXPIRE) HELLO(2)` · legacy alias `SUBSTR`(=GETRANGE)

### Phase 2
Lists: `LPUSH RPUSH LPUSHX RPUSHX LPOP RPOP LMPOP LLEN LRANGE LINDEX LSET LINSERT LREM LTRIM LMOVE RPOPLPUSH LPOS`
Hashes: `HSET HSETNX HGET HMGET HGETALL HDEL HLEN HEXISTS HKEYS HVALS HINCRBY HINCRBYFLOAT HSTRLEN HRANDFIELD HSCAN`
Sets: `SADD SREM SISMEMBER SMISMEMBER SMEMBERS SCARD SPOP SRANDMEMBER SMOVE SINTER SINTERCARD SUNION SDIFF SINTERSTORE SUNIONSTORE SDIFFSTORE SSCAN`
ZSets: `ZADD ZREM ZSCORE ZMSCORE ZCARD ZINCRBY ZRANK ZREVRANK ZRANGE ZRANGESTORE ZRANGEBYSCORE ZREVRANGEBYSCORE ZRANGEBYLEX ZREVRANGEBYLEX ZCOUNT ZLEXCOUNT ZPOPMIN ZPOPMAX ZMPOP ZRANDMEMBER ZREMRANGEBYRANK ZREMRANGEBYSCORE ZREMRANGEBYLEX ZSCAN` · optional: `LCS SORT SORT_RO ZUNION ZINTER ZDIFF ZUNIONSTORE ZINTERSTORE ZDIFFSTORE ZINTERCARD`
Deprecated aliases clients still send (each is a one-liner over the modern form — add them or your diff harness fails on real client libraries): `HMSET` (=HSET), `BRPOPLPUSH` (=BLMOVE RIGHT LEFT), `RPOPLPUSH` (=LMOVE RIGHT LEFT), `SETNX`/`SETEX`/`PSETEX`/`GETSET`, `SUBSTR` (=GETRANGE), `ZRANGEBYSCORE`-family (=ZRANGE forms)

### Phase 3
`SAVE BGSAVE BGREWRITEAOF LASTSAVE SHUTDOWN(NOSAVE|SAVE)` · `CONFIG SET appendfsync|save|appendonly` · `-LOADING` behavior · `DEBUG RELOAD`

### Phase 4
`CONFIG SET maxmemory|maxmemory-policy|maxmemory-samples` · `-OOM` on writes under noeviction · `OBJECT FREQ` real under lfu · `INFO stats: evicted_keys` · `MEMORY USAGE` (approx) · `DEBUG RECOUNT-MEMORY` (yours)

### Phase 5
`REPLICAOF/SLAVEOF(host port|NO ONE) PSYNC REPLCONF(listening-port|capa|ACK|GETACK) WAIT ROLE` · `-READONLY` · `INFO replication` complete both roles · `DEBUG DIGEST` (yours)

### Phase 6
`MULTI EXEC DISCARD WATCH UNWATCH RESET` · `BLPOP BRPOP BLMOVE BLMPOP BZPOPMIN BZPOPMAX BZMPOP WAIT` · `SUBSCRIBE UNSUBSCRIBE PSUBSCRIBE PUNSUBSCRIBE PUBLISH PUBSUB(CHANNELS|NUMSUB|NUMPAT)` · `CONFIG SET notify-keyspace-events`

### Phase 7
`CLUSTER(MYID|INFO|SHARDS|SLOTS|KEYSLOT|MEET|FORGET|RESET|BUMPEPOCH|SETSLOT|GETKEYSINSLOT|COUNTKEYSINSLOT|NODES|REPLICATE|FAILOVER)` · `MIGRATE DUMP RESTORE ASKING READONLY READWRITE` · `-MOVED -ASK -CROSSSLOT -TRYAGAIN` · stretch: `SSUBSCRIBE SUNSUBSCRIBE SPUBLISH PUBSUB(SHARDCHANNELS|SHARDNUMSUB)`

### Phase 8 (menu-dependent)
`GEOADD GEOPOS GEODIST GEOHASH GEOSEARCH GEOSEARCHSTORE` · `XADD XRANGE XREVRANGE XLEN XREAD XTRIM XDEL XSETID XGROUP XREADGROUP XACK XPENDING XCLAIM XAUTOCLAIM XINFO` · `HEXPIRE HPEXPIRE HEXPIREAT HPEXPIREAT HTTL HPTTL HEXPIRETIME HPEXPIRETIME HPERSIST HGETEX HGETDEL` · `SORT SORT_RO LCS` · `EVAL EVAL_RO EVALSHA EVALSHA_RO SCRIPT(LOAD|EXISTS|FLUSH|KILL) FUNCTION(LOAD|LIST|DUMP|DELETE|STATS) FCALL FCALL_RO` · `HELLO 3` + push · `CLIENT(TRACKING|KILL|PAUSE|UNPAUSE|NO-EVICT|NO-TOUCH)` · `AUTH ACL(...)` · `SLOWLOG(GET|LEN|RESET) LATENCY(HISTORY|RESET|LATEST)` · `SETBIT GETBIT BITCOUNT BITOP BITPOS BITFIELD BITFIELD_RO` · `PFADD PFCOUNT PFMERGE` · `WAITAOF FAILOVER` · `TIME MOVE SWAPDB LOLWUT OBJECT(REFCOUNT) MEMORY(USAGE|STATS|DOCTOR)` · `SENTINEL(...)` in dice-sentinel

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
| io-threads | `networking.c:handleClientsWithPendingReadsUsingThreads` |
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
| protections | `config.c` (registry), `networking.c` (output buffer limits in `checkClientOutputBufferLimits`) |

---

# Appendix D — Reading list, ranked

**Tier 1 — read during the project (scheduled in chapters; URLs are the stable `redis.io/topics/<slug>` short links, which redirect to the current docs):**
1. Redis official docs, the six phase-specs: RESP — `redis.io/topics/protocol` · Persistence — `redis.io/topics/persistence` · Replication — `redis.io/topics/replication` · Cluster spec — `redis.io/topics/cluster-spec` (+ tutorial `redis.io/topics/cluster-tutorial`) · Transactions — `redis.io/topics/transactions` · Eviction/LRU — `redis.io/topics/lru-cache` (each ~1 h, each is *the* spec for a phase)
2. More official docs, per feature as you build it: memory optimization — `redis.io/topics/memory-optimization` · keyspace notifications — `redis.io/topics/notifications` · pub/sub — `redis.io/topics/pubsub` · streams — `redis.io/topics/streams-intro` · latency — `redis.io/topics/latency` · Sentinel — `redis.io/topics/sentinel` · security — `redis.io/topics/security` · every command's page at `redis.io/commands/<name>` (the complexity line + the RETURN section are the contract your diff harness enforces)
3. `dict.c` dictScan comment · `aof.c` header comment · `replication.c` header comment · `hyperloglog.c` header comment — the four best in-code essays
4. antirez (`antirez.com`): "Redis persistence demystified" · "Random notes on improving the Redis LRU algorithm" (the sampled-LRU scatter plots)
5. Pugh 1990, "Skip Lists: A Probabilistic Alternative to Balanced Trees" — CACM 33(6); widely mirrored PDF

**Tier 2 — deepeners, after the matching phase:**
6. antirez: "Streams: a new general purpose data structure in Redis" · "Redis Sentinel and CAP" (the consistency position, stated by the author) · "Clarifications about Redis and Memcached"
7. Flajolet, Fusy, Gandouet, Meunier 2007, "HyperLogLog: the analysis of a near-optimal cardinality estimation algorithm" — `algo.inria.fr/flajolet/Publications/FlFuGaMe07.pdf`
8. Morris 1978, "Counting Large Numbers of Events in Small Registers" — CACM 21(10), 2 pages; the LFU counter's ancestor (overview: `en.wikipedia.org/wiki/Approximate_counting_algorithm`)
9. Geohash & Z-order: `en.wikipedia.org/wiki/Geohash`, `en.wikipedia.org/wiki/Z-order_curve`; Morton 1966, *A Computer Oriented Geodetic Data Base and a New Technique in File Sequencing* (IBM report — the original)
10. Karger et al. 1997, "Consistent Hashing and Random Trees" — read to answer "why does Redis Cluster use 16384 fixed slots instead of consistent hashing?" (answer: explicit ownership + cheap resharding + bitmap gossip; write your version in NOTES.md)
11. Kleppmann, *Designing Data-Intensive Applications* ch. 5 (Replication) — after Phase 5, to place Redis's choices on the map ConsulMe already gave you (ch. 8-9 there ↔ ch. 10-12 here)

**Tier 3 — context:**
12. The DragonflyDB architecture docs (`github.com/dragonflydb/dragonfly/blob/main/docs`) — the §14.4 road not taken, argued well by people who took it
13. "The C10K problem", Dan Kegel — `kegel.com/c10k.html` (historical); Beej's Guide to Network Programming — `beej.us/guide/bgnet` (if sockets ever feel fuzzy)
14. Valkey (`github.com/valkey-io/valkey`) / KeyDB release notes — what the forks change first tells you where the pressure is
15. Redis University (`university.redis.io`) — free courses; RU301 (operations) is a good post-Phase-8 victory lap

**Anti-list**: build-your-own-redis tutorials. You're past them by Phase 1, and their shortcuts (no incremental parse, no encodings, no propagation discipline) are exactly the bugs this book audits out of you.

---

# Appendix E — Self-test bank

All chapter self-checks collected, plus integration questions. Answer out loud, from memory; per-chapter lists live in their chapters — these are the cross-cutting finals:

1. Trace `SET k v EX 10` from socket bytes to disk bytes: name every buffer it rests in and every hook it fires, in order. (§16.1 — do it from memory.)
2. A key with TTL 5 s is written on the master. List *every* place that fact is recorded, on master, replica, AOF, and — after failover — the new master.
3. Enumerate everything that can delete a key (7+ paths) and verify each fires the same choke-point hooks. Which one did your audit-era code silently skip?
4. Why does the same 24-bit field serve LRU and LFU? What breaks if you switch policy on a live dataset (and why is that acceptable)?
5. Your snapshot goroutine and a `HSET` race. Walk both the buggy interleaving (in-place mutation) and the correct one (copy-on-write) at the pointer level.
6. Partial resync is granted — list the exact state on both sides justifying it. Now the backlog wrapped 1 byte past the replica's offset — what happens, precisely?
7. Why can a MULTI batch never interleave with a BLPOP wakeup, in both C Redis and your engine model — and what *different* mechanism enforces it in each?
8. `redis-cli -c` gets MOVED then ASK for the same key within a second. Reconstruct the cluster state that produces this.
9. For each: acked-write loss possible? (a) everysec crash (b) always crash (c) master dies, replica promoted (d) WAIT 1 returned 1, then master dies (e) cluster minority partition heals. Justify each in one sentence.
10. Rank by production frequency and explain: full-resync loop, output-buffer OOM, KEYS-in-prod stall, unbounded expiry backlog, split-brain write loss.

---

# Appendix F — Bug catalogue

Classics of the genre — each with symptom → cause → the defense this book builds. Your audit findings 1–18 are the first entries; these are the ones still ahead:

| Bug | Symptom | Cause | Defense |
|---|---|---|---|
| args aliasing | garbled keys only under pipelining | decoder buffer reuse across channel | copy at boundary + fuzz + -P 16 test (§30.7) |
| phantom expiry divergence | replica has keys master doesn't | replica expired independently | §7.4 rule; replica branch in expireIfNeeded; digest drill |
| missing DEL propagation | AOF replay resurrects expired/evicted keys | expiry/eviction bypassed propagation hook | one Delete func, hooks inside (§6.3); grep-all-callers audit |
| offset drift | +CONTINUE then protocol garbage | counted non-stream bytes (replies, RDB, handshake) | single feed function owns the counter (§24.4) |
| reply-before-fsync under `always` | "zero loss" config loses acked writes in crash test | reply released before fsync returned | reply-after-flush restructure; crash matrix row (§22.5) |
| snapshot future-leak | RDB contains values written after BGSAVE started | one mutator mutated in place | per-type COW table + isolation test (§22.3) |
| eviction pool corruption | wrong keys evicted; hit rate tanks | full-pool insertion drops best candidate / breaks order (your audit #3!) | property tests: sorted, keyset-parity, top-K (§23.3) |
| WATCH miss on expiry | EXEC succeeds despite watched key vanishing | expiry delete skipped touchWatchedKey | expiry uses full delete path; §25.2 test |
| wake into half-applied batch | BLPOP observes MULTI mid-flight | inline wakeup at signal time | two-phase ready_keys (§13.3); ordering test |
| full-resync loop | replicas never stabilize; master CPU pegged | output-limit kills slow replica → full sync makes it slower | right-size limits + backlog; reproduce-then-fix drill (§24.3) |
| ASK skipped ASKING | keys visible on importer pre-handover | importer accepted un-prefixed queries | refuse + test (§26.3) |
| stale-epoch slot grab | resharding + failover corrupt slot map | believed gossip bitmap without epoch check | epoch gate + UPDATE handling (§26.3) |
| accounting drift | eviction thrashes early / OOM late | a mutation path missed its SizeOf delta | DEBUG RECOUNT-MEMORY invariant after every storm (§23.5) |
| goroutine leak | RSS climbs across client churn | disconnect path missed a registry | pprof-count in done-whens since Phase 0 |
| cron starvation | expiry/rehash never run under load | maintenance behind the request channel with no fairness | ticker case in select + budget per tick (§15.3) |

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
# cluster: tests/phase7_cluster.sh up && redis-cli -c -p 7379

# ---------- observing yourself ----------
redis-cli -p 7379 SUBSCRIBE '__keyevent@0__:expired'         # watch active expiry live (Phase 6+)
go tool pprof http://localhost:7380/debug/pprof/goroutine    # if you expose pprof on a debug port — do
scripts/mergelogs.sh node*.log | less                        # ch. 30 cockpit
```

---

# Appendix H — Answer key (selected finals from Appendix E)

**1.** bytes → conn read buffer → decoder buffer → args (copied!) → engine channel → dispatch gates → setGenericCommand → store.Set (hooks: dirty++, watchers, notify) + SetExpire → effect `SET k v PXAT <abs>` → aofBuf + backlog/replica buffers → reply `+OK` to client output buffer → engine tail: aofBuf→write(→fsync per policy), output→socket. Rest points: conn buf, decoder buf, channel, aofBuf, backlog, output buf.

**3.** DEL/UNLINK · SET-family overwrite (dropping old value, TTL) · lazy expiry on lookup · active expiry cycle · eviction · empty-collection deletion after pop/rem · FLUSHDB/ALL · RENAME's target overwrite · MIGRATE's source-DEL · (replica) master's replicated DEL. All must funnel through the store's Delete/overwrite paths where hooks live. The audit-era code's GET-path expiry skipped every hook.

**6.** Granted: replica sent (replid, offset) where replid == master's replid (or replid2 within window) AND offset ≥ backlog start (`master_repl_offset - backlog_histlen`). Wrapped-by-1: offset < start → gates fail → FULLRESYNC. No byte is served from a wrapped region, ever — the check is arithmetic, not luck.

**9.** (a) yes ≤1 s (fsync cadence) · (b) no — reply gated on fsync · (c) yes — async repl, §10.6 · (d) yes — WAIT proves 1 replica *had* it, but if the *promoted* replica is a different one without it, gone; WAIT ≠ chosen-successor durability · (e) yes — minority master's partition-era writes destroyed on demotion full-resync.

**10.** Ranked: output-buffer OOM (any slow consumer, daily at scale) > KEYS-in-prod stall (human error, weekly somewhere) > full-resync loop (undersized limits meet big datasets) > unbounded expiry backlog (mass-TTL events) > split-brain loss (needs partition + failover + writes — rare, worst).

*(The rest are your work — that's the point. Verify against the oracle and the source, not against an answer key.)*

---

# Appendix I — Resource index by chapter

## Set up your library first (30 min, do this today)

```bash
# 1. Redis source — already at ~/Code/Learning/redis  (make -j8 once; you need the binaries)
# 2. Papers directory: skiplists-pugh.pdf, morris-counting-1978.pdf, hyperloglog-flajolet.pdf
# 3. Docs offline: redis.io/docs → the six spec pages (Appendix D tier 1) — save as PDF; you'll read each 2-3×
# 4. Reference implementations to READ (never import):
#    github.com/redis/redis (have it) · github.com/valkey-io/valkey (the fork, actively diverging)
#    github.com/dragonflydb/dragonfly (the §14.4 alternative, C++) · github.com/tidwall/redcon (Go RESP server lib — read AFTER Phase 0 to compare parsers)
```

## Per chapter (beyond the read-alongside boxes)

- **ch. 2**: `ae.c` is the text. Compare with your `async_tcp.go` side by side — write the diff-of-designs in NOTES.md.
- **ch. 5**: after your dict works, read `dictScan`'s comment again; it reads differently once you own a two-table dict.
- **ch. 9**: `tests/integration/aof*.tcl`, `rdb.tcl` — steal test cases wholesale for the crash matrix.
- **ch. 10**: `tests/integration/replication*.tcl` — likewise, especially `replication-psync.tcl`'s parameterized matrix.
- **ch. 12**: the cluster spec page > the code for concepts; the code > the spec for gossip packet details.
- **ch. 28**: real `redis.conf` (repo root) — read every comment once; it is the best-documented config file in open source and half of it is production wisdom in disguise.

## Per phase — implementation references

| Phase | Spec / oracle material |
|---|---|
| 0 | `redis.io/topics/protocol` · `networking.c` (Session 3) · `github.com/tidwall/redcon` (compare parsers *after* yours works) · `tests/unit/protocol.tcl` |
| 1 | `redis.io/commands/{set,expire,scan,object}` · `tests/unit/{expire,keyspace,scan,type/string}.tcl` · Sessions 4–7 |
| Stage 3 | `dict.c`/`listpack.c`/`t_zset.c`/`intset.c` headers (the formats ARE the spec) · Pugh 1990 |
| 2 | `redis.io/commands` per family · `tests/unit/type/{list,hash,set,zset}.tcl` (steal cases wholesale) · Session 12 |
| 3 | `redis.io/topics/persistence` · `rdb.h` opcodes · `tests/integration/{aof,rdb,aof-multi-part}.tcl` · ch. 29 · Sessions 9–10 |
| 4 | `redis.io/topics/lru-cache` · antirez LRU-notes post · `tests/unit/maxmemory.tcl` · Session 8 |
| 5 | `redis.io/topics/replication` · `tests/integration/replication*.tcl` (esp. `-psync`) · Session 11 · DDIA ch. 5 after |
| 6 | `redis.io/topics/{transactions,pubsub,notifications}` · `tests/unit/{multi,pubsub,keyspace-events?}.tcl` · Session 13 |
| 7 | `redis.io/topics/cluster-spec` (the assignment) · `tests/unit/cluster/*.tcl` · Session 14 · your ConsulMe SWIM notes |
| 8 | per menu row: `redis.io/topics/streams-intro` · Flajolet 2007 · Geohash/Z-order wiki + `geohash_helper.c` · `redis.io/topics/{sentinel,latency,security}` |

## The order, condensed (pin above your desk)

```
ch.0-3 → PHASE 0 → ch.6-7 → PHASE 1 → ch.4-5 → STAGE 3 (ds libs) → s.12 → PHASE 2
      → ch.9,29 + s.9,10 → PHASE 3 → ch.8 + s.8 → PHASE 4
      → ch.10 + s.11 → PHASE 5 → ch.13 + s.13 → PHASE 6
      → ch.12 + s.14 → PHASE 7 → ch.11,28 → PHASE 8 / hardening
      (ch.14 in week one; ch.15-16 after Phase 1; ch.30 before Phase 5)
```

---

# Closing

Two projects now sit side by side on your disk. ConsulMe taught you what it costs to *refuse* to lose data — quorums, fsync-before-vote, the wall that is Raft. DiceMe teaches what you can buy by *agreeing* to lose a little — a replication stream with no election safety, an eviction that guesses, an expiry that samples, a snapshot that forks. Same networks, same crashes, opposite bets. When you can argue both sides — when "why doesn't Redis just use Raft?" and "why doesn't Consul just use a backlog?" are both questions you answer with trade-offs instead of slogans — you have the thing interviews probe for and incidents demand: judgment about consistency, not vocabulary about it.

## Then what — after DiceMe

- **Valkey/Redis source contribution** — with this project done, the real codebase is legible end to end; the test suites you stole from are the on-ramp.
- **The DragonflyDB question** — redo §14.4's decision the other way: shard the keyspace across cores with shared-nothing engines and see which chapter-7-through-13 semantics you must re-earn. That's a serious systems project with this book as its prerequisite.
- **Streams + a real workload** — build a job queue on your own XADD/XREADGROUP and run it under chaos; consumer-group edge cases are a course in exactly-once mythology.
- **Write the retrospective** — the audit table (before) vs the final architecture (after), benchmarks vs C Redis with the gaps explained, the bug catalogue. That document is the proof-of-work; the repo is just its evidence.

Build the checkpoint. Run the storm. Kill -9 it. Trust nothing you haven't crashed.

