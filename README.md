# DiceMe

A Redis-compatible in-memory key-value store built from scratch in Go — following the Redis source code as a reference to understand how each piece actually works under the hood.

---

## What's implemented

| Layer | Details |
|---|---|
| **Networking** | Async TCP server using kqueue (macOS), non-blocking I/O, graceful shutdown via SIGTERM/SIGINT |
| **Protocol** | Full RESP encoder and decoder — simple strings, errors, integers, bulk strings, arrays |
| **Commands** | `PING`, `SET` (with `EX`), `GET`, `TTL`, `DEL`, `EXPIRE`, `INCR`, `INFO`, `BGREWRITEAOF`, `SLEEP` |
| **Storage** | In-memory hash map with a separate expiry map keyed by object pointer |
| **Type encoding** | `OBJ_TYPE_STRING` with three encodings: `INT`, `EMBSTR` (≤44 bytes), `RAW` |
| **Eviction** | Three strategies: `simple-first`, `allkeys-random`, approximate LRU with a 16-slot eviction pool |
| **Persistence** | AOF (Append-Only File) write via `BGREWRITEAOF` |
| **Expiry** | Active expiry on `GET`; passive background sweep via `DeleteExpiredKeys` running every 1s |

---

## Known issues (structural)

**1. Busy-spin in `WaitForSignal` (`async_tcp.go`)**
`for atomic.LoadInt32(&eStatus) == EngineStatus_BUSY {}` burns 100% of a core while waiting for the engine to go idle. Add `runtime.Gosched()` or a short sleep inside the loop.

**2. `expireSample()` walks the full store instead of the `expires` map (`expire.go`)**
Iterating `store` to find expired keys is O(all keys). The `expires` map already holds only the keys with a TTL set — iterate that instead.

**3. `EvictionPool.Push()` sorts the entire pool on every insertion (`evictionpool.go`)**
`sort.Sort(ByIdleTime(pq.pool))` is O(n log n) on every push. The pool is bounded at 16 items — a single insertion-sort pass is sufficient, or use `container/heap` for a proper priority queue.

---

## Roadmap — next features

Implement these in order.

### 1. Key utility commands
`EXISTS`, `KEYS` (glob pattern), `TYPE`, `RENAME`, `PERSIST`

Reference: `db.c` in the Redis source. `KEYS` requires glob matching; `TYPE` reads `TypeEncoding`; `RENAME` teaches atomic swap semantics across the store and expiry maps.

### 2. More string commands
`MSET`, `MGET`, `GETSET`, `SETNX`, `APPEND`, `STRLEN`

Reference: `t_string.c`. `SETNX` teaches conditional-put. `APPEND` forces encoding promotion — when you append a non-numeric suffix to an INT-encoded value it must upgrade to RAW.

### 3. AOF load on startup
Implement `LoadAOF()` — open the AOF file on server start, decode the RESP arrays, replay each command through `EvalAndRespond`. Completes the persistence story and reuses the RESP decoder already written. Bug #4 must be fixed before this works correctly.

### 4. List data structure
`LPUSH`, `RPUSH`, `LPOP`, `RPOP`, `LLEN`, `LRANGE`

Reference: `t_list.c`. Add `OBJ_TYPE_LIST` with `OBJ_ENCODING_LISTPACK` for small lists and `OBJ_ENCODING_LINKEDLIST` for large ones. This introduces the encoding-upgrade pattern used by all compound types.

### 5. Hash data structure
`HSET`, `HGET`, `HDEL`, `HGETALL`, `HLEN`

Reference: `t_hash.c`. Small hashes use listpack; large ones promote to a Go `map`. Builds directly on the encoding-upgrade pattern introduced by List.

---

## Running

```bash
go run main.go
# or with flags
go run main.go -host 0.0.0.0 -port 7379
```

Connect with any Redis client:

```bash
redis-cli -p 7379
```

## Project layout

```
.
├── config/         # host, port, keys limit, eviction strategy, AOF path
├── core/
│   ├── store.go        # Put / Get / Del, expiry map
│   ├── object.go       # Obj struct, type/encoding constants
│   ├── typeencoding.go # getType, getEncoding, assertType, assertEncoding
│   ├── type_string.go  # deduceTypeEncoding
│   ├── eval.go         # command evaluation, EvalAndRespond
│   ├── resp.go         # RESP encode / decode
│   ├── expire.go       # hasExpired, expireSample, DeleteExpiredKeys
│   ├── eviction.go     # evictFirst, evictAllkeysRandom, evictAllkeysLRU
│   ├── evictionpool.go # approximate LRU pool
│   ├── aof.go          # DumpAllAOF
│   ├── stats.go        # KeyspaceStat
│   ├── events.go       # Shutdown hook
│   └── comm.go         # FDComm (syscall read/write wrapper)
├── server/
│   ├── async_tcp.go    # kqueue event loop, graceful shutdown state machine
│   └── sync_tcp.go     # blocking TCP server (reference)
└── main.go
```
