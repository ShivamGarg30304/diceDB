package core

import (
	"time"

	"github.com/shivam30303/diceDB/config"
)

var store map[string]*Obj
var expires map[string]uint64

func init() {
	store = make(map[string]*Obj)
	expires = make(map[string]uint64)
}

func setExpiry(k string, expDurationMs int64) {
	expires[k] = uint64(time.Now().UnixMilli()) + uint64(expDurationMs)
}

func NewObj(value interface{}, oType uint8, oEnc uint8) *Obj {
	return &Obj{
		Value:          value,
		TypeEncoding:   oType | oEnc,
		LastAccessedAt: getCurrentClock(),
	}
}

func Put(k string, obj *Obj) {
	if len(store) >= config.KeysLimit {
		evict()
	}
	_, exists := store[k]

	obj.LastAccessedAt = getCurrentClock()
	store[k] = obj

	// overwriting a key drops any timeout it carried, matching SET semantics
	delete(expires, k)

	if KeyspaceStat[0] == nil {
		KeyspaceStat[0] = make(map[string]int)
	}
	if !exists {
		KeyspaceStat[0]["keys"]++
	}
}

func Get(k string) *Obj {
	v := store[k]
	if v != nil {
		if hasExpired(k) {
			Del(k)
			return nil
		}
		v.LastAccessedAt = getCurrentClock()
	}

	return v
}

func Del(k string) bool {
	if _, ok := store[k]; ok {
		delete(store, k)
		delete(expires, k)
		KeyspaceStat[0]["keys"]--
		return true
	}
	return false
}

func Rename(oldKey, newKey string) {
	obj := store[oldKey]
	exp, hasExp := expires[oldKey]

	Put(newKey, obj)
	if hasExp {
		expires[newKey] = exp
	}
	Del(oldKey)
}
