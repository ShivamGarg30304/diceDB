package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"time"
)

var RESP_NIL []byte = []byte("$-1\r\n")
var RESP_OK []byte = []byte("+OK\r\n")
var RESP_ZERO []byte = []byte(":0\r\n")
var RESP_ONE []byte = []byte(":1\r\n")
var RESP_MINUS_1 []byte = []byte(":-1\r\n")
var RESP_MINUS_2 []byte = []byte(":-2\r\n")

func evalPING(args []string) []byte {
	var b []byte

	if len(args) >= 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'ping' command"), false)
	}

	if len(args) == 0 {
		b = Encode("PONG", true)
	} else {
		b = Encode(args[0], false)
	}

	return b
}

func evalSET(args []string) []byte {
	if len(args) <= 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'set' command"), false)
	}

	var key, value string
	var exDurationMs int64 = -1

	key, value = args[0], args[1]
	oType, oEnc := deduceTypeEncoding(value)

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "EX", "ex":
			i++
			if i == len(args) {
				return Encode(errors.New("ERR syntax error"), false)
			}

			exDurationSec, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil {
				return Encode(errors.New("ERR value is not an integer or out of range"), false)
			}
			exDurationMs = exDurationSec * 1000
		default:
			return Encode(errors.New("ERR syntax error"), false)
		}
	}

	// putting the k and value in a Hash Table
	obj := NewObj(value, oType, oEnc)
	Put(key, obj)
	if exDurationMs > 0 {
		setExpiry(key, exDurationMs)
	}
	return RESP_OK
}

func evalGET(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'get' command"), false)
	}

	var key string = args[0]

	// Get the key from the hash table
	obj := Get(key)

	// if key does not exist, return RESP encoded nil
	if obj == nil {
		return RESP_NIL
	}

	// if key already expired then return nil
	if hasExpired(key) {
		return RESP_NIL
	}

	// return the RESP encoded value
	return Encode(obj.Value, false)
}

func evalTTL(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'ttl' command"), false)
	}

	var key string = args[0]

	obj := Get(key)

	// if key does not exist, return RESP encoded -2 denoting key does not exist
	if obj == nil {
		return RESP_MINUS_2
	}

	// if object exist, but no expiration is set on it then send -1
	exp, isExpirySet := getExpiry(key)
	if !isExpirySet {
		return RESP_MINUS_1
	}

	// if key expired i.e. key does not exist hence return -2
	if exp < uint64(time.Now().UnixMilli()) {
		return RESP_MINUS_2
	}

	// compute the time remaining for the key to expire and
	// return the RESP encoded form of it
	durationMs := exp - uint64(time.Now().UnixMilli())

	// if key expired i.e. key does not exist hence return -2
	return Encode(int64(durationMs/1000), false)
}

func evalDEL(args []string) []byte {
	var countDeleted int = 0

	for _, key := range args {
		if ok := Del(key); ok {
			countDeleted++
		}
	}

	return Encode(countDeleted, false)
}

func evalEXPIRE(args []string) []byte {
	if len(args) <= 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'expire' command"), false)
	}

	var key string = args[0]
	exDurationSec, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}

	obj := Get(key)

	// 0 if the timeout was not set. e.g. key doesn't exist, or operation skipped due to the provided arguments
	if obj == nil {
		return RESP_ZERO
	}

	setExpiry(key, exDurationSec*1000)

	// 1 if the timeout was set.
	return RESP_ONE
}

// TODO: Make it async by forking a new process
func evalBGREWRITEAOF(args []string) []byte {
	DumpAllAOF()
	return RESP_OK
}

func evalINCR(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'incr' command"), false)
	}

	var key string = args[0]
	obj := Get(key)
	if obj == nil {
		obj = NewObj("0", OBJ_TYPE_STRING, OBJ_ENCODING_INT)
		Put(key, obj)
	}

	if err := assertType(obj.TypeEncoding, OBJ_TYPE_STRING); err != nil {
		return Encode(err, false)
	}

	if err := assertEncoding(obj.TypeEncoding, OBJ_ENCODING_INT); err != nil {
		return Encode(err, false)
	}

	i, _ := strconv.ParseInt(obj.Value.(string), 10, 64)
	i++
	obj.Value = strconv.FormatInt(i, 10)

	return Encode(i, false)
}

func evalINFO(args []string) []byte {
	var info []byte
	buf := bytes.NewBuffer(info)
	buf.WriteString("# Keyspace\r\n")
	for i := range KeyspaceStat {
		buf.WriteString(fmt.Sprintf("db%d:keys=%d,expires=%d,avg_ttl=0\r\n", i, KeyspaceStat[i]["keys"], len(expires)))
	}
	return Encode(buf.String(), false)
}

func evalCLIENT(args []string) []byte {
	return RESP_OK
}

func evalLATENCY(args []string) []byte {
	return Encode([]string{}, false)
}

func evalLRU(args []string) []byte {
	evictAllkeysLRU()
	return RESP_OK
}

func evalSLEEP(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'SLEEP' command"), false)
	}

	durationSec, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}
	time.Sleep(time.Duration(durationSec) * time.Second)
	return RESP_OK
}

func evalEXISTS(args []string) []byte {
	if len(args) == 0 {
		return Encode(errors.New("ERR wrong number of arguments for 'exists' command"), false)
	}

	var count int = 0

	for _, key := range args {
		if Get(key) != nil {
			count++
		}
	}
	return Encode(count, false)
}

func evalKEYS(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'keys' command"), false)
	}

	pattern := args[0]
	var keys []string

	for k := range store {
		if hasExpired(k) {
			continue
		}

		matched, err := path.Match(pattern, k)
		if err != nil {
			return Encode(errors.New("ERR invalid pattern"), false)
		}
		if matched {
			keys = append(keys, k)
		}
	}

	return Encode(keys, false)
}

func evalTYPE(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'type' command"), false)
	}

	key := args[0]
	obj := Get(key)
	if obj == nil {
		return Encode("none", true)
	}

	objType := getType(obj.TypeEncoding)
	switch objType {
	case OBJ_TYPE_STRING:
		return Encode("string", true)
	default:
		return Encode("unknown", true)
	}
}

func evalRENAME(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'rename' command"), false)
	}

	oldKey := args[0]
	newKey := args[1]

	oldObj := store[oldKey]
	if oldObj == nil || hasExpired(oldKey) {
		return Encode(errors.New("ERR no such key"), false)
	}

	if oldKey == newKey {
		return RESP_OK
	}

	Rename(oldKey, newKey)

	return RESP_OK
}

func evalPERSIST(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'persist' command"), false)
	}

	key := args[0]

	obj := store[key]
	if obj == nil || hasExpired(key) {
		return RESP_ZERO
	}

	removeExpiry(key)

	return RESP_ONE
}

func EvalAndRespond(cmds RedisCmds, c io.ReadWriter) {
	var response []byte
	buf := bytes.NewBuffer(response)

	for _, cmd := range cmds {
		switch cmd.Cmd {
		case "PING":
			buf.Write(evalPING(cmd.Args))
		case "SET":
			buf.Write(evalSET(cmd.Args))
		case "GET":
			buf.Write(evalGET(cmd.Args))
		case "TTL":
			buf.Write(evalTTL(cmd.Args))
		case "DEL":
			buf.Write(evalDEL(cmd.Args))
		case "EXPIRE":
			buf.Write(evalEXPIRE(cmd.Args))
		case "BGREWRITEAOF":
			buf.Write(evalBGREWRITEAOF(cmd.Args))
		case "INCR":
			buf.Write(evalINCR(cmd.Args))
		case "INFO":
			buf.Write(evalINFO(cmd.Args))
		case "CLIENT":
			buf.Write(evalCLIENT(cmd.Args))
		case "LATENCY":
			buf.Write(evalLATENCY(cmd.Args))
		case "LRU":
			buf.Write(evalLRU(cmd.Args))
		case "SLEEP":
			buf.Write(evalSLEEP(cmd.Args))
		case "EXISTS":
			buf.Write(evalEXISTS(cmd.Args))
		case "KEYS":
			buf.Write(evalKEYS(cmd.Args))
		case "TYPE":
			buf.Write(evalTYPE(cmd.Args))
		case "RENAME":
			buf.Write(evalRENAME(cmd.Args))
		case "PERSIST":
			buf.Write(evalPERSIST(cmd.Args))
		default:
			buf.Write(evalPING(cmd.Args))
		}
	}
	c.Write(buf.Bytes())
}
