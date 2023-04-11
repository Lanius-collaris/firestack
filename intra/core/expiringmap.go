// Copyright (c) 2023 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package core

import (
	"sync"
	"time"
)

var (
	reapthreshold = 5 * time.Minute
	maxreapiter   = 100
	sizethreshold = 500
)

type val struct {
	expiry time.Time
	hits   uint32
}

type ExpMap struct {
	sync.Mutex // guards ExpMap.
	m          map[string]*val
	lastreap   time.Time
}

func NewExpiringMap() *ExpMap {
	m := &ExpMap{
		m:        make(map[string]*val),
		lastreap: time.Now(),
	}
	return m
}

func (m *ExpMap) Get(key string) uint32 {
	n := time.Now()

	m.Lock()
	defer m.Unlock()

	v, ok := m.m[key]
	if !ok {
		v = &val{
			expiry: n,
		}
		m.m[key] = v
	} else if n.After(v.expiry) {
		v.hits = 0
	} else {
		v.hits += 1
	}
	return v.hits
}

func (m *ExpMap) Set(key string, expiry time.Duration) uint32 {
	n := time.Now().Add(expiry)

	m.Lock()
	defer m.Unlock()

	v, ok := m.m[key]
	if ok && n.After(v.expiry) {
		v.expiry = n
	} else {
		v = &val{
			expiry: n,
		}
		m.m[key] = v
	}

	go m.reaper()

	return v.hits
}

func (m *ExpMap) Delete(key string) {
	m.Lock()
	defer m.Unlock()

	delete(m.m, key)
}

func (m *ExpMap) Len() int {
	m.Lock()
	defer m.Unlock()

	return len(m.m)
}

func (m *ExpMap) Clear() int {
	m.Lock()
	defer m.Unlock()

	l := len(m.m)
	m.m = make(map[string]*val)
	return l
}

func (m *ExpMap) reaper() {
	m.Lock()
	defer m.Unlock()

	l := len(m.m)
	if l < sizethreshold {
		return
	}

	now := time.Now()
	treap := m.lastreap.Add(reapthreshold)
	// if last reap was reap-threshold minutes ago...
	if now.Sub(treap) <= 0 {
		return
	}
	m.lastreap = now
	// reap up to maxreapiter entries
	i := 0
	for k, v := range m.m {
		i += 1
		if now.Sub(v.expiry) > 0 {
			delete(m.m, k)
		}
		if i > maxreapiter {
			break
		}
	}
}
