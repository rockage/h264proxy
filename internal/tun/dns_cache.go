package tun

import (
	"context"
	"encoding/binary"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsCacheCapacity = 512
const dnsCacheMaxTTL = 5 * time.Minute

type dnsCacheEntry struct {
	wire    []byte // immutable, complete upstream answer; each reader gets its own copy
	stored  time.Time
	expires time.Time
}

type dnsFlight struct {
	done chan struct{}
	wire []byte
	err  error
}

func cacheableQuery(q dnsmessage.Message) bool {
	// Include the entire query (except ID) in the key. DO/CD bits, record type,
	// question case and all EDNS options must remain distinct. Signed requests
	// and nonstandard records must not have transaction IDs rewritten.
	if len(q.Answers) != 0 || len(q.Authorities) != 0 {
		return false
	}
	for _, r := range q.Additionals {
		if r.Header.Type != dnsmessage.TypeOPT {
			return false
		}
	}
	return true
}

func answerWithID(wire []byte, id uint16) []byte {
	result := append([]byte(nil), wire...)
	if len(result) >= 2 {
		binary.BigEndian.PutUint16(result, id)
	}
	return result
}

func (e dnsCacheEntry) answer(id uint16, now time.Time) []byte {
	if !now.Before(e.expires) {
		return nil
	}
	var m dnsmessage.Message
	if m.Unpack(e.wire) != nil {
		return nil
	}
	age := uint32(now.Sub(e.stored) / time.Second)
	m.Header.ID = id
	for _, section := range [][]dnsmessage.Resource{m.Answers, m.Authorities, m.Additionals} {
		for i := range section {
			if section[i].Header.Type == dnsmessage.TypeOPT {
				continue
			} // EDNS flags, not a TTL
			ttl := section[i].Header.TTL
			if age >= ttl {
				return nil
			}
			section[i].Header.TTL = ttl - age
		}
	}
	wire, err := m.Pack()
	if err != nil {
		return nil
	}
	return wire
}

func cacheEntry(wire []byte, now time.Time) (dnsCacheEntry, bool) {
	var m dnsmessage.Message
	if m.Unpack(wire) != nil || m.Header.Truncated || m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) == 0 {
		return dnsCacheEntry{}, false
	}
	ttl := uint32(dnsCacheMaxTTL / time.Second)
	for _, section := range [][]dnsmessage.Resource{m.Answers, m.Authorities, m.Additionals} {
		for _, r := range section {
			if r.Header.Type == dnsmessage.TypeOPT {
				continue
			}
			if r.Header.TTL < ttl {
				ttl = r.Header.TTL
			}
		}
	}
	if ttl == 0 {
		return dnsCacheEntry{}, false
	}
	return dnsCacheEntry{wire: append([]byte(nil), wire...), stored: now, expires: now.Add(time.Duration(ttl) * time.Second)}, true
}

// resolve combines simultaneous identical lookups without coupling their caller
// cancellations. The lookup itself is bounded and canceled on frontend shutdown.
func (p *dnsProxy) resolve(ctx context.Context, query []byte, q dnsmessage.Message) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := string(query[2:])
	p.resolverMu.Lock()
	if entry, ok := p.cache[key]; ok {
		if a := entry.answer(q.Header.ID, time.Now()); a != nil {
			p.resolverMu.Unlock()
			return a, nil
		}
		delete(p.cache, key)
	}
	f := p.flights[key]
	if f == nil {
		f = &dnsFlight{done: make(chan struct{})}
		if p.flights == nil {
			p.flights = make(map[string]*dnsFlight)
		}
		p.flights[key] = f
		query = append([]byte(nil), query...)
		p.lookupWG.Add(1)
		go func() {
			defer p.lookupWG.Done()
			parent := p.lookupCtx
			if parent == nil {
				parent = context.Background()
			}
			lookupCtx, cancel := context.WithTimeout(parent, 15*time.Second)
			defer cancel()
			wire, err := p.fetch(lookupCtx, query, q)
			p.resolverMu.Lock()
			defer p.resolverMu.Unlock()
			if err == nil {
				if entry, ok := cacheEntry(wire, time.Now()); ok {
					if p.cache == nil {
						p.cache = make(map[string]dnsCacheEntry)
					}
					if len(p.cache) >= dnsCacheCapacity {
						// Discard expired entries first; cap memory even for distinct queries.
						now := time.Now()
						for k, e := range p.cache {
							if !now.Before(e.expires) {
								delete(p.cache, k)
							}
						}
						if len(p.cache) >= dnsCacheCapacity {
							for k := range p.cache {
								delete(p.cache, k)
								break
							}
						}
					}
					p.cache[key] = entry
				}
			}
			f.wire, f.err = wire, err
			delete(p.flights, key)
			close(f.done)
		}()
	}
	p.resolverMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return answerWithID(f.wire, q.Header.ID), nil
	}
}
