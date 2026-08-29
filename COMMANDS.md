# The Redis command surface — what to learn, and when

Companion to `THE_BOOK.md`. Complexities and version numbers are taken from `~/Code/Learning/redis/src/commands/*.json` at commit `d22066d09`, which is the authoritative source — that directory is what generates `commands.def`, `COMMAND INFO`, and the docs on redis.io.

**How to use this file.** Section 1 is the only part to read today; it is the twelve command pages chapter 1 asks for. Everything after it is a per-build-chapter checklist — open the section when you start that chapter, not before. Section 14 is the complexity cheat sheet, and section 15 lists what you can safely ignore.

Notation: **★** means read the full doc page at `redis.io/commands/<name>`. A command with no note next to it is exactly what it sounds like and needs no commentary.

---

## 1. Read these twelve now (chapter 1, ~45 min)

The point of this exercise is not to memorise commands. It is to internalise that **Redis publishes the cost of every operation and expects you to be the query planner** (§1.2). Read the complexity line and the RETURN section on each page; skip the examples.

| # | Command | Complexity | What to notice |
|---|---|---|---|
| 1 | ★ `SET` | O(1) | The option matrix — `NX XX GET EX PX EXAT PXAT KEEPTTL` — is the biggest one you build (ch. 11), and `SETNX`/`SETEX`/`PSETEX`/`GETSET` are all one-liners over it |
| 2 | ★ `GET` | O(1) | The baseline. ~100 ns of work wrapped in 50–500 µs of network (§1.1) |
| 3 | ★ `EXPIRE` | O(1) | Read the "how Redis expires keys" appendix at the bottom — it is the specification for chapter 9 |
| 4 | ★ `DEL` | O(1) for a string, **O(M) for a collection** | The complexity is in the number of *elements*, not keys. Deleting one 10M-entry hash blocks the server. This single line is why `UNLINK` and lazy-free exist (§1.4, §2.2) |
| 5 | ★ `LPUSH` | O(1) per element | |
| 6 | ★ `LRANGE` | **O(S+N)** | S is the distance of the start offset from the nearest end. `LRANGE mylist 500000 500010` returns 11 elements and costs half a million steps. Cost depends on *where*, not just *how much* |
| 7 | ★ `HGETALL` | O(N) in the hash size | The command everyone calls on a 100k-field hash exactly once, in production, at peak |
| 8 | ★ `SMEMBERS` | O(N) in set cardinality | Same lesson, different type. `SSCAN` is the answer |
| 9 | ★ `ZADD` | O(log N) per item | The skiplist showing through the API. Note the flag matrix `NX XX GT LT CH INCR` — you port its truth table verbatim in ch. 19 |
| 10 | ★ `ZRANGEBYSCORE` | **O(log N + M)** | log N to find the start, M to walk. The shape of every range query in the book, and why zsets use a skiplist rather than a hash |
| 11 | ★ `KEYS` | O(N) in the whole keyspace | The docs tell you not to use it in production, in bold, and then ship it anyway. The cardinal sin (§1.4) |
| 12 | ★ `SCAN` | **O(1) per call**, O(N) per full iteration | The apology for `KEYS`. Read its guarantee section carefully — "every element present for the whole iteration is returned at least once, duplicates are possible" is a contract you must reproduce exactly (§15.3) |

Bonus, if you want the extreme: ★ `SORT` is `O(N + M·log M)` — the worst complexity in Redis and the closest thing it has to a join.

Those twelve cover all four cost shapes: constant, logarithmic, linear, and amortised-constant-per-call. Everything else in this file is a variation.

---

## 2. Chapter 7 — connection and protocol

No data commands. This chapter is the transport, so its surface is tiny.

| Command | Complexity | Note |
|---|---|---|
| `PING` | O(1) | Two forms: bare (`+PONG`) and with a message (bulk echo). In subscriber mode under RESP2 it replies differently — diff it |
| `ECHO` | O(1) | |
| `QUIT` | O(1) | Deprecated since 7.2 but every client still sends it. Reply `+OK`, then close |
| `HELLO [2\|3] [AUTH u p] [SETNAME n]` | O(1) | The protocol negotiation. Modern clients send this **first, on every connection**. Its reply is a map of server/version/proto/id/mode/role/modules |
| `RESET` | O(1) | Unwinds everything: MULTI state, WATCHes, subscriber mode, client name, protocol version, authentication. Connection pools call it before returning a connection. Missing it produces bizarre cross-request bugs in *someone else's* code |
| `DEBUG SLEEP` | O(1) | Your first `DEBUG` subcommand. It grows every build chapter |

Plus the dispatch errors, which are commands in every way that matters to a client: `-ERR unknown command 'x', with args beginning with:` and the arity error, byte for byte.

---

## 3. Chapter 11 — strings, keys, and the server surface

### Strings

| Command | Complexity | Note |
|---|---|---|
| `SET` | O(1) | The full matrix. `SET k v NX GET` is legal since 7.0; it was an error before. Do not guess, diff |
| `GET` | O(1) | |
| `GETDEL` | O(1) | 6.2 |
| `GETEX` | O(1) | 6.2. Sets or clears the TTL as a side effect of a read — `GETEX k PERSIST` is a write |
| `APPEND` | Amortised O(1) | **Forces `raw` encoding.** An int- or embstr-encoded value cannot be appended in place (§8.2). Classic missed detail |
| `STRLEN` | O(1) | |
| `SETRANGE` | O(1) amortised, O(M) in the argument | Zero-fills any gap. `SETRANGE k 10000000 x` allocates 10 MB |
| `GETRANGE` | O(N) in the returned length | Negative indexes count from the end, and both ends are **inclusive** |
| `INCR` `DECR` `INCRBY` `DECRBY` | O(1) | Overflow must error with `ERR increment or decrement would overflow`, and a non-integer value with `ERR value is not an integer or out of range`. Your audit finding #10 |
| `INCRBYFLOAT` | O(1) | Formats with `%.17g` then trims trailing zeros. **Propagates as `SET k <result>`** (§20.4) so float arithmetic never re-runs on a replica |
| `MSET` `MGET` `MSETNX` | O(N) in key count | `MSETNX` is all-or-nothing |
| `SETNX` `SETEX` `PSETEX` `GETSET` | O(1) | Deprecated, and every client library still sends them. One line each over `SET`'s matrix — that *is* the lesson |
| `SUBSTR` | O(N) | Deprecated alias of `GETRANGE`. Accept it |
| `LCS` | **O(N·M) time and memory** | 7.0. Two 1 MB strings ask for a terabyte-scale matrix, and real Redis refuses past a limit. Building it teaches §1.2 from the inside |

### Key management

| Command | Complexity | Note |
|---|---|---|
| `DEL` | O(1) string, O(M) collection | |
| `UNLINK` | O(1), reclamation in a background thread | 4.0. In Go the GC *is* the background thread, so this is an alias — but know why it exists |
| `EXISTS` | O(N) in key count | `EXISTS k k k` returns 3. Repeats count |
| `TYPE` | O(1) | |
| `TOUCH` | O(N) | Updates LRU/LFU without reading the value |
| `COPY` | O(1) string, O(N) collection | 6.2. `DB` and `REPLACE` options. Must deep-copy, and must **not** copy the TTL unless asked |
| `RENAME` `RENAMENX` | O(1) | A store-level swap, never a capacity-checked insert — your audit finding #13. The TTL travels with the key. `ERR no such key` on a missing source |
| `KEYS` | O(N) | Needs your own glob, ported from `util.c:stringmatchlen`. `path.Match` is wrong: it treats `/` specially and errors on malformed patterns where Redis matches them literally (audit #14) |
| `SCAN` | O(1) per call | `MATCH`, `COUNT`, `TYPE`. The cursor is a decimal string; `0` terminates. Reverse-binary cursor (§15.3) |
| `RANDOMKEY` | O(1) | Must be honestly random. Go's `for k := range m { break }` is famously biased — another reason you build the dict |
| `TTL` `PTTL` | O(1) | `PTTL` is exact milliseconds; **`TTL` rounds to nearest second**, so 900 ms left is `1`, not `0`. Returns `-1` for no TTL, `-2` for no key |
| `EXPIRE` `PEXPIRE` `EXPIREAT` `PEXPIREAT` | O(1) | With `NX XX GT LT` (7.0). **A non-positive TTL deletes the key immediately and propagates as `DEL`** (audit #7) |
| `EXPIRETIME` `PEXPIRETIME` | O(1) | 7.0. Returns the absolute deadline instead of the remaining time |
| `PERSIST` | O(1) | Returns whether it actually removed a TTL |
| `DBSIZE` | O(1) | |
| `FLUSHDB` `FLUSHALL` | O(N) | `ASYNC`/`SYNC` options |
| `SELECT` | O(1) | All 16 databases (§8.4). Cluster mode allows only 0 |
| `MOVE` | O(1) | Move a key to another database |
| `SWAPDB` | O(1) | Swap two whole databases by pointer |
| `SORT` `SORT_RO` | O(N + M·log M) | `BY pattern`, `GET pattern`, `ALPHA`, `LIMIT`, `STORE`. Pattern dereference (`weight_*`, `obj_*->field`, the literal `#`) is a real parser exercise. With `BY` it is **not deterministic**, so with `STORE` it propagates as its effect |

### Server and introspection

| Command | Complexity | Note |
|---|---|---|
| `CONFIG GET\|SET\|RESETSTAT` | O(N) | Glob patterns in `GET`. Registry-backed: name, type, default, validator, dynamic flag, apply hook |
| `INFO [section\|all\|everything]` | O(1) | Sections: server, clients, memory, persistence, stats, replication, cpu, keyspace, **commandstats, errorstats, latencystats**. Exporters parse these by field name; a missing field is a broken dashboard |
| `COMMAND` | O(N) | Full metadata for every command |
| `COMMAND COUNT` | O(1) | |
| `COMMAND INFO <name…>` | O(N) | Arity, flags, first/last/step key positions, ACL categories |
| `COMMAND DOCS [name…]` | O(N) | 7.0. **`redis-cli` calls this on startup** to build completion. Omitting it is visible immediately |
| `COMMAND LIST` | O(N) | 7.0 |
| `COMMAND GETKEYS <cmd> <args…>` | O(N) | Extracts key arguments from an arbitrary command. Used by cluster proxies and by your own ACL key checks in ch. 47 |
| `CLIENT ID\|GETNAME\|SETNAME\|LIST\|INFO` | O(1)/O(N) | `CLIENT LIST`'s field set is long and clients parse it |
| `OBJECT ENCODING\|IDLETIME\|FREQ\|REFCOUNT\|HELP` | O(1) | `OBJECT ENCODING` is how your tests prove conversions happen at exactly the configured threshold |
| `TIME` | O(1) | Seconds and microseconds, two-element array |
| `DEBUG OBJECT\|SLEEP\|SET-ACTIVE-EXPIRE\|JMAP\|STRINGMATCH-LEN` | — | The real test suite depends on these existing |

---

## 4. Chapter 19 — the four collections

### Lists (listpack → quicklist)

| Command | Complexity | Note |
|---|---|---|
| `LPUSH` `RPUSH` `LPUSHX` `RPUSHX` | O(1) per element | The `X` variants only act if the key exists |
| `LPOP` `RPOP` | O(N) in the count returned | Optional count argument since 6.2 |
| `LLEN` | O(1) | |
| `LRANGE` | O(S+N) | Negative indexes; both ends inclusive |
| `LINDEX` | O(N), O(1) at either end | |
| `LSET` | O(N), O(1) at either end | |
| `LINSERT BEFORE\|AFTER` | O(N) to the pivot | Returns `-1` when the pivot is absent |
| `LREM` | O(N+M) | The count argument's sign picks the direction: positive from head, negative from tail, zero removes all |
| `LTRIM` | O(N) removed | |
| `LMOVE` `RPOPLPUSH` | O(1) | `RPOPLPUSH` is the deprecated `LMOVE RIGHT LEFT`. Both still shipped by clients |
| `LPOS` | O(N) | `RANK` (negative searches from the tail), `COUNT`, `MAXLEN` |
| `LMPOP` | O(N+M) | 7.0. First non-empty of N keys |

### Hashes (listpack → hashtable)

| Command | Complexity | Note |
|---|---|---|
| `HSET` | O(1) per pair | Variadic since 4.0 |
| `HSETNX` | O(1) | |
| `HGET` | O(1) | |
| `HMGET` | O(N) requested | |
| `HGETALL` | O(N) | |
| `HDEL` | O(N) removed | **Deleting the last field deletes the key** |
| `HLEN` `HEXISTS` `HSTRLEN` | O(1) | |
| `HKEYS` `HVALS` | O(N) | |
| `HINCRBY` `HINCRBYFLOAT` | O(1) | |
| `HRANDFIELD` | O(N) returned | 6.2. Negative count allows duplicates; positive count does not |
| `HSCAN` | O(1) per call | `NOVALUES` option since 7.4 |
| `HMSET` | O(N) | Deprecated alias of `HSET`. Accept it |

### Sets (intset → listpack → hashtable)

| Command | Complexity | Note |
|---|---|---|
| `SADD` `SREM` | O(1) / O(N) | Last member removed deletes the key |
| `SISMEMBER` | O(1) | |
| `SMISMEMBER` | O(N) | 6.2 |
| `SMEMBERS` | O(N) | |
| `SCARD` | O(1) | |
| `SPOP` | O(1), O(N) with count | **Propagates as `SREM <the actual member>`** — randomness must not re-run on the replica (§20.4) |
| `SRANDMEMBER` | O(1), O(N) with count | Negative count allows duplicates and can return more than the set size. Read the doc before writing the test |
| `SMOVE` | O(1) | |
| `SINTER` `SINTERSTORE` | O(N·M) | |
| `SINTERCARD` | O(N·M) | 7.0. With a `LIMIT` that lets it stop early |
| `SUNION` `SUNIONSTORE` `SDIFF` `SDIFFSTORE` | O(N) total elements | |
| `SSCAN` | O(1) per call | |

### Sorted sets (listpack → skiplist + dict)

| Command | Complexity | Note |
|---|---|---|
| `ZADD` | O(log N) per item | `NX XX GT LT CH INCR`. Some combinations are errors — port the truth table from `zsetAdd`, do not re-derive it |
| `ZREM` | O(M·log N) | |
| `ZSCORE` | O(1) | Via the companion dict, not the skiplist |
| `ZMSCORE` | O(N) | 6.2 |
| `ZCARD` | O(1) | |
| `ZINCRBY` | O(log N) | |
| `ZRANK` `ZREVRANK` | O(log N) | **Only if you implement skiplist spans.** Without them it is O(N), and most tutorials skip them (§15.6). `WITHSCORE` option since 7.2 |
| `ZRANGE` | O(log N + M) | The unified 6.2 form: `REV`, `BYSCORE`, `BYLEX`, `LIMIT` |
| `ZRANGEBYSCORE` `ZREVRANGEBYSCORE` `ZRANGEBYLEX` `ZREVRANGEBYLEX` `ZREVRANGE` | O(log N + M) | Deprecated in favour of `ZRANGE`, still sent by every client |
| `ZRANGESTORE` | O(log N + M) | 6.2 |
| `ZCOUNT` `ZLEXCOUNT` | O(log N) | |
| `ZPOPMIN` `ZPOPMAX` | O(log N · M) | 5.0 |
| `ZRANDMEMBER` | O(N) returned | 6.2 |
| `ZREMRANGEBYRANK\|BYSCORE\|BYLEX` | O(log N + M) | |
| `ZSCAN` | O(1) per call | |
| `ZUNION` `ZINTER` `ZDIFF` (+`STORE`) | O(N·K) or O(N)+O(M log M) | `WEIGHTS` and `AGGREGATE SUM\|MIN\|MAX` |
| `ZINTERCARD` | O(N·K) | 7.0 |
| `ZMPOP` | O(K) + O(M·log N) | 7.0 |

**Score and range syntax**, which is where this chapter's bugs live: `+inf` / `-inf`, `(3.5` for an exclusive bound, and lex ranges using `[a`, `(b`, `-`, `+`. If you are stuck on a range-boundary diff for an hour, you are inclusive/exclusive-swapped on one end.

**The empty-collection rule applies to all four types**: a collection that becomes empty is deleted, so `EXISTS` returns 0 and `TYPE` returns `none`. Forgetting it diverges `DBSIZE`, `EXISTS` and `TYPE` everywhere at once.

---

## 5. Chapter 23 — persistence

| Command | Complexity | Note |
|---|---|---|
| `SAVE` | O(N) | Blocking, by design. Build it first — the format debugged in peace |
| `BGSAVE` | O(N) in a child | `SCHEDULE` option. In Go, your snapshot goroutine (§23.3) |
| `BGREWRITEAOF` | O(N) | |
| `LASTSAVE` | O(1) | |
| `SHUTDOWN [NOSAVE\|SAVE\|NOW\|FORCE\|ABORT]` | O(N) | |
| `DUMP` | O(1) + O(N·M) | RDB value payload + 2-byte version footer + CRC64 |
| `RESTORE` | O(1) + O(N·M), O(N·M·log N) for zsets | `REPLACE`, `ABSTTL`, `IDLETIME`, `FREQ` |
| `DEBUG RELOAD` | O(N) | Save and reload in place. The cheapest possible round-trip test, used constantly by the real test suite |
| `DEBUG LOADAOF` | O(N) | |

Plus the `-LOADING Redis is loading the dataset in memory` error for connections that arrive during boot.

---

## 6. Chapter 26 — memory and eviction

| Command | Complexity | Note |
|---|---|---|
| `MEMORY USAGE key [SAMPLES n]` | O(N) | Your `SizeOf()` with a sampling rule for large collections |
| `OBJECT FREQ` | O(1) | Only meaningful under an LFU policy |
| `CONFIG SET maxmemory\|maxmemory-policy\|maxmemory-samples\|maxmemory-clients` | O(1) | |
| `DEBUG RECOUNT-MEMORY` | O(N) | Yours, not Redis's. Walks and re-sums; assert drift is zero after every storm run |

And the error every write must produce when over budget under `noeviction`: `-OOM command not allowed when used memory > 'maxmemory'`. Reads keep working.

---

## 7. Chapter 30 — replication

| Command | Complexity | Note |
|---|---|---|
| `REPLICAOF host port` / `REPLICAOF NO ONE` | O(1) | `SLAVEOF` is the deprecated spelling; accept both |
| `PSYNC <replid> <offset>` | — | Replies `+FULLRESYNC <replid> <offset>` or `+CONTINUE <replid>` |
| `REPLCONF listening-port\|capa\|ACK\|GETACK` | O(1) | Internal, but a replica is just a client, so it is a real command |
| `WAIT numreplicas timeout` | O(1) | Blocks until N replicas ack an offset. Synchronous replication on demand — **not** consensus |
| `ROLE` | O(1) | Returns `master`/`slave` plus offsets and the peer list. The pre-`INFO` way clients discover topology; three lines once the state exists |
| `DEBUG DIGEST` / `DEBUG DIGEST-VALUE` | O(N) | Order-independent digest of the keyspace. The real test suite compares a master's digest to a replica's, never to a constant, so it only has to be self-consistent |

Plus `-READONLY You can't write against a read only replica.` on any write to a replica.

---

## 8. Chapter 33 — transactions, blocking, pub/sub

| Command | Complexity | Note |
|---|---|---|
| `MULTI` `EXEC` `DISCARD` | O(1) | A **queue-time** error (unknown command, bad arity) aborts `EXEC` with `-EXECABORT`. A **runtime** error does not: everything else still executes, errors are returned in position, and there is no rollback |
| `WATCH` `UNWATCH` | O(1) | Optimistic locking. Fires on every effective write **including expiry and eviction deletes**, because those go through the same choke point |
| `BLPOP` `BRPOP` | O(N) in key count | Blocks the *client*, never the server. FIFO among waiters. Propagates as the `LPOP` it eventually performs |
| `BLMOVE` `BRPOPLPUSH` | O(1) | `BRPOPLPUSH` is the deprecated spelling of `BLMOVE RIGHT LEFT` |
| `BLMPOP` | O(N+M) | 7.0 |
| `BZPOPMIN` `BZPOPMAX` | O(log N) | 5.0. Same registry, different pop function — proof the choke point generalises |
| `BZMPOP` | O(K) + O(M·log N) | 7.0 |
| `SUBSCRIBE` `UNSUBSCRIBE` | O(N) | Under RESP2 a subscribed client enters restricted mode; under RESP3 there is no restriction and messages arrive as push frames |
| `PSUBSCRIBE` `PUNSUBSCRIBE` | O(N) | Patterns are matched per publish — O(patterns), unavoidable |
| `PUBLISH` | O(N+M) | Fire and forget. No buffering, no replay, no acks. Propagates through the replication stream so subscribers on replicas hear it |
| `PUBSUB CHANNELS\|NUMSUB\|NUMPAT` | O(N) | |

Blocking timeouts are floats with millisecond resolution since 6.0. `BLPOP` inside `MULTI` does **not** block — it degrades to the non-blocking form. Diff that.

---

## 9. Chapter 36 — cluster

| Command | Complexity | Note |
|---|---|---|
| `CLUSTER KEYSLOT` | O(N) in key length | `CRC16(key) mod 16384`, honouring `{hash tags}` |
| `CLUSTER SHARDS` `CLUSTER SLOTS` | O(N) | `SLOTS` is deprecated in favour of `SHARDS`; clients still bootstrap from both |
| `CLUSTER NODES` | O(N) | The text format clients and `redis-cli --cluster` parse |
| `CLUSTER MYID` `CLUSTER INFO` | O(1) | |
| `CLUSTER MEET` `CLUSTER FORGET` `CLUSTER RESET` | O(1) | |
| `CLUSTER SETSLOT` | O(1) | `IMPORTING`, `MIGRATING`, `STABLE`, `NODE` |
| `CLUSTER GETKEYSINSLOT` `CLUSTER COUNTKEYSINSLOT` | O(N) | |
| `CLUSTER REPLICATE` `CLUSTER REPLICAS` `CLUSTER FAILOVER` | O(1) | `FAILOVER` takes `FORCE` and `TAKEOVER` |
| `CLUSTER BUMPEPOCH` `CLUSTER SET-CONFIG-EPOCH` | O(1) | |
| `MIGRATE` | `DUMP` + `DEL` + `RESTORE` + O(N) transfer | Synchronous per batch. `KEYS` form for batching |
| `ASKING` | O(1) | One-shot prefix. An importer **must refuse** slot queries that arrive without it |
| `READONLY` `READWRITE` | O(1) | Lets a client read from a cluster replica |
| `SSUBSCRIBE` `SUNSUBSCRIBE` `SPUBLISH` | O(N) | 7.0. Sharded pub/sub, routed by `slot(channel)`. Client libraries in cluster mode use these by default |
| `PUBSUB SHARDCHANNELS\|SHARDNUMSUB` | O(N) | |

The errors are as much a part of the protocol as the commands: `-MOVED <slot> <host:port>`, `-ASK <slot> <host:port>`, `-CROSSSLOT Keys in request don't hash to the same slot`, `-TRYAGAIN`, `-CLUSTERDOWN`.

---

## 10. Chapter 40 — scripting

| Command | Complexity | Note |
|---|---|---|
| `EVAL script numkeys key… arg…` | Depends on the script | Keys must be declared: cluster slot-checks them, ACLs authorise them. Outside cluster mode Redis does *not* enforce that you only touch declared keys |
| `EVAL_RO` | — | Refuses writes, so a replica can serve it |
| `EVALSHA` `EVALSHA_RO` | — | `-NOSCRIPT` on a cache miss; the client library re-sends the body |
| `SCRIPT LOAD\|EXISTS\|FLUSH\|KILL` | O(N) | The cache is per-server, not persisted, not replicated |
| `FUNCTION LOAD\|LIST\|DELETE\|FLUSH\|DUMP\|RESTORE\|STATS\|KILL` | O(N) | 7.0. Libraries **are** persisted in the RDB and replicated — unlike the script cache |
| `FCALL` `FCALL_RO` | — | |

Errors that are part of the contract: `-BUSY Redis is busy running a script...` and `-UNKILLABLE` once the script has written.

---

## 11. Chapter 42 — streams

| Command | Complexity | Note |
|---|---|---|
| `XADD` | O(1), O(N) when trimming | **Propagates with the generated ID**, never `*` — get this wrong and master and replica diverge permanently |
| `XLEN` | O(1) | |
| `XRANGE` `XREVRANGE` | O(N) returned | `-` and `+` bounds, `(` for exclusive since 6.2 |
| `XDEL` | O(1) per ID | Leaves a tombstone; IDs never shift |
| `XTRIM` | O(N) evicted | `MAXLEN` / `MINID`, exact or `~` approximate with `LIMIT`. `~` trims whole macro-nodes only — that is what makes it cheap |
| `XSETID` | O(1) | With `ENTRIESADDED` and `MAXDELETEDID` |
| `XREAD` | O(N) returned | `$` means "only what arrives after this call". `BLOCK` reuses the ch. 33 registry |
| `XGROUP CREATE\|CREATECONSUMER\|DELCONSUMER\|DESTROY\|SETID` | O(1), `DESTROY` is O(N) in the PEL | `MKSTREAM` on `CREATE` |
| `XREADGROUP` | O(M) returned | `>` delivers new entries; an explicit ID re-delivers that consumer's own pending ones. `NOACK` skips the PEL |
| `XACK` | O(1) per ID | |
| `XPENDING` | O(1) summary, O(N) extended | |
| `XCLAIM` | O(log N) in the PEL | `IDLE`, `TIME`, `RETRYCOUNT`, `FORCE`, `JUSTID` |
| `XAUTOCLAIM` | O(1) with small `COUNT` | 6.2. Cursor-driven. Its **third** reply element is the list of IDs it had to drop because they no longer exist — easy to omit, and clients use it |
| `XINFO STREAM [FULL]\|GROUPS\|CONSUMERS` | O(1) | |

---

## 12. Chapter 44 — bitmaps, HyperLogLog, geospatial

| Command | Complexity | Note |
|---|---|---|
| `SETBIT` `GETBIT` | O(1) | Bit 0 is the **most** significant bit of byte 0. `SETBIT k 10000000 1` zero-fills 1.25 MB |
| `BITCOUNT` | O(N) | Range unit defaults to `BYTE`; `BIT` since 7.0. `BITCOUNT k 0 0` gives different answers under each |
| `BITPOS` | O(N) | Same range units |
| `BITOP AND\|OR\|XOR\|NOT` | O(N) | Zero-extends shorter operands. `NOT` takes exactly one source |
| `BITFIELD` `BITFIELD_RO` | O(1) per subcommand | Types `u1…u63` / `i1…i64`, `#`-prefixed offsets in type-width units, and `OVERFLOW WRAP\|SAT\|FAIL` as a **mode switch** affecting subsequent operations, not an argument to one |
| `PFADD` | O(1) per element | Returns 1 only if the estimate actually changed |
| `PFCOUNT` | O(1) single key, O(N) multi-key | Multi-key merges on the fly without storing |
| `PFMERGE` | O(N) | Register-wise maximum |
| `PFDEBUG` `PFSELFTEST` | — | Internal, and the real test suite calls them |
| `GEOADD` | O(log N) per item | `NX`, `XX`, `CH`. It is a plain `ZADD` with a Morton-interleaved 52-bit score |
| `GEOPOS` | O(1) per member | Lossy at ~0.6 m — your diff harness needs a tolerance rule here, one of the few legitimate ones |
| `GEODIST` | O(1) | Haversine, with a unit argument |
| `GEOHASH` | O(1) per member | Returns the **standard 11-character base-32 geohash**, which is a different encoding from the internal score. Common trip-up |
| `GEOSEARCH` | O(N + log M) | `FROMMEMBER`/`FROMLONLAT`, `BYRADIUS`/`BYBOX`, `ASC`/`DESC`, `COUNT n [ANY]`, `WITHCOORD`/`WITHDIST`/`WITHHASH` |
| `GEOSEARCHSTORE` | O(N + log M) | `STOREDIST` stores distances instead of geohashes |
| `GEORADIUS` `GEORADIUSBYMEMBER` (+`_RO`) | O(N + log M) | Deprecated, still in every client. The non-`_RO` forms accept `STORE`/`STOREDIST`, which makes them **write** commands despite the name |

A HyperLogLog is a plain string with a `"HYLL"` header, so it must be byte-compatible: other tools and other servers read these blobs. Use Redis's exact hash function or nothing interoperates.

---

## 13. Chapters 47 and 48 — security, admin, and the last of it

| Command | Complexity | Note |
|---|---|---|
| `AUTH [username] password` | O(1) | Single-argument form sets the `default` user's password since 6.0 |
| `ACL SETUSER\|GETUSER\|DELUSER\|LIST\|USERS\|CAT\|WHOAMI\|GENPASS\|LOG\|LOAD\|SAVE\|DRYRUN` | O(N) | Rules apply **left to right**; `+@all -get` and `-get +@all` differ. `ACL LOG` is the audit trail operators ask for first |
| `CLIENT KILL` | O(N) | Filters: `ID`, `ADDR`, `LADDR`, `TYPE`, `USER`, `SKIPME`, `MAXAGE` |
| `CLIENT PAUSE` `CLIENT UNPAUSE` | O(1) | `WRITE`/`ALL` modes. Under the engine model: stop dequeuing. Also a failover primitive |
| `CLIENT NO-EVICT` `CLIENT NO-TOUCH` | O(1) | |
| `CLIENT TRACKING` `CLIENT TRACKINGINFO` `CLIENT CACHING` | O(1) | Client-side caching. `BCAST`, `OPTIN`, `OPTOUT`, `PREFIX`, `NOLOOP`, `REDIRECT`. Invalidations fire from the keyspace choke point |
| `HEXPIRE` `HPEXPIRE` `HEXPIREAT` `HPEXPIREAT` | O(N) fields | 7.4. Per-*field* TTLs: the chapter-9 contract one level down, including the replica rule |
| `HTTL` `HPTTL` `HEXPIRETIME` `HPEXPIRETIME` `HPERSIST` | O(N) fields | 7.4 |
| `HGETEX` `HGETDEL` | O(N) fields | 8.0 |
| `WAITAOF` | O(1) | 7.2. Blocks on fsync offsets rather than replica offsets |
| `FAILOVER [TO host port] [ABORT] [TIMEOUT ms]` | O(1) | Coordinated, lossless manual failover: pause writes, wait for catch-up, swap roles. `WAIT` + `CLIENT PAUSE` + `PSYNC` composed |
| `SLOWLOG GET\|LEN\|RESET\|HELP` | O(N) | Ring buffer of slow calls |
| `LATENCY HISTORY\|RESET\|LATEST\|DOCTOR` | O(1) | |
| `MEMORY USAGE\|STATS\|DOCTOR\|PURGE` | O(N) | |
| `LOLWUT` | O(1) | Yes, really. It is in the test suite |

Sentinel is its own binary and its own surface (`SENTINEL masters\|replicas\|sentinels\|get-master-addr-by-name\|is-master-down-by-addr\|reset\|failover\|ckquorum\|monitor\|remove\|set\|flushconfig`), covered in chapters 37 and 45.

---

## 14. Complexity cheat sheet

Everything in Redis is one of these six shapes. If you can classify a command on sight, you have got what section 1 was for.

| Shape | Meaning | Examples |
|---|---|---|
| **O(1)** | One hash lookup or one pointer move | `GET`, `SET`, `HGET`, `ZSCORE`, `LLEN`, `SCARD`, `EXPIRE` |
| **O(log N)** | Skiplist descent | `ZADD`, `ZRANK`, `ZINCRBY`, `GEOADD` |
| **O(log N + M)** | Find the start, then walk M results | Every zset range command |
| **O(N) in the value** | Touches every element of one value | `HGETALL`, `SMEMBERS`, `LRANGE 0 -1`, `DEL` on a collection |
| **O(N) in the keyspace** | Touches every key. **Never on the command path in production** | `KEYS`, `FLUSHALL`, `RANDOMKEY`'s honest version |
| **O(1) amortised per call** | Bounded work per call, unbounded total | `SCAN` and its `HSCAN`/`SSCAN`/`ZSCAN` siblings, `XAUTOCLAIM` |

The two that catch people: `LRANGE` is **O(S+N)** — the offset costs, not just the count — and `DEL` is **O(M) in elements**, which is the whole reason `UNLINK` exists.

---

## 15. What you can skip

**Too new for any client library** (this tree is 8.9.241, ahead of what anything in the wild speaks). Do not build these; nothing will ask for them:

`DELEX`, `INCREX`, `MSETEX`, `DIGEST` (string), `LMOVEM`, `BLMOVEM`, `SDIFFCARD`, `SUNIONCARD`, `HIMPORT` (all subcommands), `HSETEX`, `XACKDEL`, `XDELEX`, `XNACK`, `XCFGSET`, `XIDMPRECORD`.

**Out of scope permanently:** `MODULE LOAD\|UNLOAD\|LIST` — a Go server cannot load a C shared object built against Redis's internal ABI (§48.4).

**Deprecated, but you must still accept them.** Every one is a one-liner over its modern form, and every one is still sent by production client libraries. Skipping them fails chapter 48, not chapter 19:

| Deprecated | Modern equivalent |
|---|---|
| `SETNX` `SETEX` `PSETEX` `GETSET` | `SET` with options |
| `SUBSTR` | `GETRANGE` |
| `HMSET` | `HSET` |
| `RPOPLPUSH` | `LMOVE RIGHT LEFT` |
| `BRPOPLPUSH` | `BLMOVE RIGHT LEFT` |
| `ZRANGEBYSCORE` `ZREVRANGEBYSCORE` `ZRANGEBYLEX` `ZREVRANGEBYLEX` `ZREVRANGE` | `ZRANGE` with `BYSCORE`/`BYLEX`/`REV` |
| `GEORADIUS` `GEORADIUSBYMEMBER` (+`_RO`) | `GEOSEARCH` / `GEOSEARCHSTORE` |
| `SLAVEOF` | `REPLICAOF` |
| `CLUSTER SLOTS` | `CLUSTER SHARDS` |
| `QUIT` | closing the socket |
