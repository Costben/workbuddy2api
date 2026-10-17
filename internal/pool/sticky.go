package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

// StickyEligibleUIDsForModelRealm keeps healthy busy accounts eligible for an
// existing binding. New bindings still use the capacity-aware available list.
func (p *Pool) StickyEligibleUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if (realm == "" || e.a.Realm() == realm) && e.healthyForModel(now, model) {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	return uids
}

// AcquireStickyForModelRealm returns (nil,true) for a healthy busy account and
// (nil,false) for an ineligible account. A nonnil result owns one request lease.
func (p *Pool) AcquireStickyForModelRealm(uid, model, realm string) (*auth.Auth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	now := time.Now()
	if !ok || (realm != "" && e.a.Realm() != realm) || !e.healthyForModel(now, model) {
		return nil, false
	}
	limit := p.inFlightLimit(e)
	for {
		cur := e.inFlight.Load()
		if limit > 0 && cur >= int64(limit) {
			return nil, true
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			break
		}
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a, true
}
