package state

import (
	"bytes"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/common/hexutil"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/turbo/shards"
	"github.com/ledgerwatch/turbo-geth/turbo/trie"
)

// CachedReader is a wrapper for an instance of type StateReader
// This wrapper only makes calls to the underlying reader if the item is not in the cache
type CachedReader struct {
	r     StateReader
	cache *shards.StateCache
}

// NewCachedReader wraps a given state reader into the cached reader
func NewCachedReader(r StateReader, cache *shards.StateCache) *CachedReader {
	return &CachedReader{r: r, cache: cache}
}

const ReadStateByPrefixes = true

// ReadAccountData is called when an account needs to be fetched from the state
func (cr *CachedReader) ReadAccountData(address common.Address) (*accounts.Account, error) {
	addrBytes := address.Bytes()
	if a, ok := cr.cache.GetAccount(addrBytes); ok {
		return a, nil
	}

	if !ReadStateByPrefixes {
		a, err := cr.r.ReadAccountData(address)
		if err != nil {
			return nil, err
		}
		if a == nil {
			cr.cache.SetAccountAbsent(addrBytes)
		} else {
			cr.cache.SetAccountRead(addrBytes, a)
		}
		return a, nil
	}

	var hashed common.Hash
	h := common.NewHasher()
	defer common.ReturnHasherToPool(h)
	h.Sha.Reset()
	_, _ = h.Sha.Write(addrBytes)
	_, _ = h.Sha.Read(hashed[:])
	var hashedNibbles []byte
	hexutil.DecompressNibbles(hashed[:], &hashedNibbles)
	// TODO: if hasTree but no such ihK in cache - then need load this part of trie from disk to cache
	ihK, hasState, alreadyLoaded, trieMiss := cr.cache.FindDeepestAccountTrie(hashedNibbles[:])
	if trieMiss {
		if err := cr.r.(*PlainStateReader).db.Walk(dbutils.TrieOfAccountsBucket, ihK, len(ihK)*8, func(k, v []byte) (bool, error) {
			hasState, hasTree, hasHash, newV := trie.UnmarshalTrieNodeTyped(v)
			cr.cache.SetAccountTrieRead(k, hasState, hasTree, hasHash, newV)
			return true, nil
		}); err != nil {
			return nil, err
		}
		ihK, hasState, alreadyLoaded, trieMiss = cr.cache.FindDeepestAccountTrie(hashedNibbles[:])
	}

	if ihK == nil {
		a, err := cr.r.ReadAccountData(address)
		if err != nil {
			return nil, err
		}
		if a == nil {
			cr.cache.SetAccountAbsent(addrBytes)
		} else {
			cr.cache.SetAccountRead(addrBytes, a)
		}
		return a, nil
	}
	if !hasState || alreadyLoaded {
		cr.cache.SetAccountAbsent(addrBytes)
		return nil, nil
	}
	buf := common.CopyBytes(ihK)
	fixedBits := len(buf) * 4
	if len(buf)%2 == 1 {
		buf = append(buf, 0)
	}
	hexutil.CompressNibbles(buf, &buf)
	found := false
	var a *accounts.Account
	if err := cr.r.(*PlainStateReader).db.Walk(dbutils.HashedAccountsBucket, buf, fixedBits, func(k, v []byte) (bool, error) {
		acc, ok := cr.cache.GetAccountByHashedAddress(common.BytesToHash(k))
		if ok {
			if bytes.Equal(k, hashed[:]) {
				found = true
				a = acc
			}
			return true, nil
		}
		acc = new(accounts.Account)
		if err := acc.DecodeForStorage(v); err != nil {
			return false, err
		}
		cr.cache.DeprecatedSetAccountRead(common.BytesToHash(k), acc)
		if bytes.Equal(k, hashed[:]) {
			found = true
			a = acc
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	if !found {
		cr.cache.SetAccountAbsent(addrBytes)
	}
	cr.cache.MarkAccountTrieAsLoaded(ihK)
	return a, nil
}

// ReadAccountStorage is called when a storage item needs to be fetched from the state
func (cr *CachedReader) ReadAccountStorage(address common.Address, incarnation uint64, key *common.Hash) ([]byte, error) {
	addrBytes := address.Bytes()
	if s, ok := cr.cache.GetStorage(addrBytes, incarnation, key.Bytes()); ok {
		return s, nil
	}
	v, err := cr.r.ReadAccountStorage(address, incarnation, key)
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		cr.cache.SetStorageAbsent(addrBytes, incarnation, key.Bytes())
	} else {
		cr.cache.SetStorageRead(addrBytes, incarnation, key.Bytes(), v)
	}
	return v, nil
}

// ReadAccountCode iws called when code of an account needs to be fetched from the state
// Usually, one of (address;incarnation) or codeHash is enough to uniquely identify the code
func (cr *CachedReader) ReadAccountCode(address common.Address, incarnation uint64, codeHash common.Hash) ([]byte, error) {
	if bytes.Equal(codeHash[:], emptyCodeHash) {
		return nil, nil
	}
	if c, ok := cr.cache.GetCode(address.Bytes(), incarnation); ok {
		return c, nil
	}
	c, err := cr.r.ReadAccountCode(address, incarnation, codeHash)
	if err != nil {
		return nil, err
	}
	if cr.cache != nil && len(c) <= 1024 {
		cr.cache.SetCodeRead(address.Bytes(), incarnation, c)
	}
	return c, nil
}

func (cr *CachedReader) ReadAccountCodeSize(address common.Address, incarnation uint64, codeHash common.Hash) (int, error) {
	c, err := cr.ReadAccountCode(address, incarnation, codeHash)
	return len(c), err
}

// ReadAccountIncarnation is called when incarnation of the account is required (to create and recreate contract)
func (cr *CachedReader) ReadAccountIncarnation(address common.Address) (uint64, error) {
	deleted := cr.cache.GetDeletedAccount(address.Bytes())
	if deleted != nil {
		return deleted.Incarnation, nil
	}
	return cr.r.ReadAccountIncarnation(address)
}
