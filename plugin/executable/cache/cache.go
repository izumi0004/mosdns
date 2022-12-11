/*
 * Copyright (C) 2020-2022, IrineSistiana
 *
 * This file is part of mosdns.
 *
 * mosdns is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * mosdns is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package cache

import (
	"context"
	"github.com/IrineSistiana/mosdns/v5/coremain"
	"github.com/IrineSistiana/mosdns/v5/pkg/cache"
	"github.com/IrineSistiana/mosdns/v5/pkg/dnsutils"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/pkg/utils"
	"github.com/IrineSistiana/mosdns/v5/plugin/executable/sequence"
	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/exp/constraints"
	"golang.org/x/sync/singleflight"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	PluginType = "cache"
)

func init() {
	coremain.RegNewPluginFunc(PluginType, Init, func() interface{} { return new(Args) })
}

const (
	defaultLazyUpdateTimeout = time.Second * 5
	minimumChangesToDump     = 1024
)

var _ sequence.RecursiveExecutable = (*cachePlugin)(nil)

type Args struct {
	Size              int    `yaml:"size"`
	LazyCacheTTL      int    `yaml:"lazy_cache_ttl"`
	LazyCacheReplyTTL int    `yaml:"lazy_cache_reply_ttl"`
	DumpFile          string `yaml:"dump_file"`
	DumpInterval      int    `yaml:"dump_interval"`
}

func (a *Args) init() {
	utils.SetDefaultUnsignNum(&a.Size, 1024)
	utils.SetDefaultUnsignNum(&a.LazyCacheReplyTTL, 5)
	utils.SetDefaultUnsignNum(&a.DumpInterval, 600)
}

type cachePlugin struct {
	*coremain.BP
	args *Args

	backend      *cache.Cache[key, *dns.Msg]
	lazyUpdateSF singleflight.Group
	closeOnce    sync.Once
	closeNotify  chan struct{}
	updatedKey   atomic.Uint64

	queryTotal   prometheus.Counter
	hitTotal     prometheus.Counter
	lazyHitTotal prometheus.Counter
	size         prometheus.GaugeFunc
}

func Init(bp *coremain.BP, args interface{}) (coremain.Plugin, error) {
	return newCachePlugin(bp, args.(*Args)), nil
}

func newCachePlugin(bp *coremain.BP, args *Args) *cachePlugin {
	args.init()

	backend := cache.New[key, *dns.Msg](cache.Opts{Size: args.Size})

	p := &cachePlugin{
		BP:      bp,
		args:    args,
		backend: backend,

		queryTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "query_total",
			Help: "The total number of processed queries",
		}),
		hitTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hit_total",
			Help: "The total number of queries that hit the cache",
		}),
		lazyHitTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lazy_hit_total",
			Help: "The total number of queries that hit the expired cache",
		}),
		size: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cache_size",
			Help: "Current cache size in records",
		}, func() float64 {
			return float64(backend.Len())
		}),
	}
	bp.GetMetricsReg().MustRegister(p.queryTotal, p.hitTotal, p.lazyHitTotal, p.size)

	if err := p.loadDump(); err != nil {
		p.L().Error("failed to load cache dump", zap.Error(err))
	}
	p.startDumpLoop()

	bp.RegAPI(p.api())
	return p
}

func (c *cachePlugin) Exec(ctx context.Context, qCtx *query_context.Context, next sequence.ChainWalker) error {
	c.queryTotal.Inc()
	q := qCtx.Q()

	msgKey := getMsgKey(q)
	if len(msgKey) == 0 { // skip cache
		return next.ExecNext(ctx, qCtx)
	}

	cachedResp, lazyHit := c.lookupCache(msgKey)
	if lazyHit {
		c.lazyHitTotal.Inc()
		c.doLazyUpdate(msgKey, qCtx, next)
	}
	if cachedResp != nil { // cache hit
		c.hitTotal.Inc()
		cachedResp.Id = q.Id // change msg id
		shuffleIP(cachedResp)
		qCtx.SetResponse(cachedResp)
	}

	err := next.ExecNext(ctx, qCtx)

	// This response is not from cache. Cache it.
	if r := qCtx.R(); cachedResp == nil && r != nil {
		c.tryStoreMsg(msgKey, r)
	}
	return err
}

// doLazyUpdate starts a new goroutine to execute next node and update the cache in the background.
// It has an inner singleflight.Group to de-duplicate same msgKey.
func (c *cachePlugin) doLazyUpdate(msgKey string, qCtx *query_context.Context, next sequence.ChainWalker) {
	qCtxCopy := qCtx.Copy()
	lazyUpdateFunc := func() (interface{}, error) {
		defer c.lazyUpdateSF.Forget(msgKey)
		qCtx := qCtxCopy

		c.L().Debug("start lazy cache update", qCtx.InfoField())
		ctx, cancel := context.WithTimeout(context.Background(), defaultLazyUpdateTimeout)
		defer cancel()

		err := next.ExecNext(ctx, qCtx)
		if err != nil {
			c.L().Warn("failed to update lazy cache", qCtx.InfoField(), zap.Error(err))
		}

		r := qCtx.R()
		if r != nil {
			c.tryStoreMsg(msgKey, r)
		}
		c.L().Debug("lazy cache updated", qCtx.InfoField())
		return nil, nil
	}
	c.lazyUpdateSF.DoChan(msgKey, lazyUpdateFunc) // DoChan won't block this goroutine
}

// lookupCache returns the cached response. The ttl of returned msg will be changed properly.
// Note: Caller SHOULD change the msg id because it's not same as query's.
func (c *cachePlugin) lookupCache(msgKey string) (r *dns.Msg, lazyHit bool) {
	// lookup in cache
	v, storedTime, _, _ := c.backend.Get(key(msgKey))

	// cache hit
	if v != nil {
		r = v.Copy()
		msgTTL := time.Duration(dnsutils.GetMinimalTTL(r)) * time.Second

		// Not expired.
		if storedTime.Add(msgTTL).After(time.Now()) {
			dnsutils.SubtractTTL(r, uint32(time.Since(storedTime).Seconds()))
			return r, false
		}

		// Expired but lazy update enabled and cached response has valid answer.
		if c.args.LazyCacheTTL > 0 && r.Rcode == dns.RcodeSuccess && len(r.Answer) > 0 {
			dnsutils.SetTTL(r, uint32(c.args.LazyCacheReplyTTL))
			return r, true
		}
		// Expired negative response (NXDOMAIN, etc. ) should not be used.
	}

	// cache miss
	return nil, false
}

// tryStoreMsg tries to store r to cache. If r should be cached.
func (c *cachePlugin) tryStoreMsg(msgKey string, r *dns.Msg) {
	if r.Truncated != false {
		return
	}

	// Set different ttl for different responses.
	var ttl time.Duration
	switch r.Rcode {
	case dns.RcodeNameError:
		ttl = time.Second * 30
	case dns.RcodeServerFailure:
		ttl = time.Second * 5
	case dns.RcodeSuccess:
		minTTL := dnsutils.GetMinimalTTL(r)
		if len(r.Answer) == 0 { // Empty answer. Set ttl between 0~300.
			const emtpyAnswerTtl = 300
			ttl = time.Duration(min(minTTL, emtpyAnswerTtl)) * time.Second
			break
		}

		if c.args.LazyCacheTTL > 0 {
			ttl = time.Duration(c.args.LazyCacheTTL) * time.Second
		} else {
			ttl = time.Duration(minTTL) * time.Second
		}
	default:
		return
	}

	v := r.Copy()
	// RFC 6891 6.2.1. Cache Behaviour
	// The OPT record MUST NOT be cached.
	dnsutils.RemoveEDNS0(v)

	c.updatedKey.Add(1)
	now := time.Now()
	c.backend.Store(key(msgKey), v, now, now.Add(ttl))
}

func (c *cachePlugin) Close() error {
	if err := c.dumpCache(); err != nil {
		c.L().Error("failed to dump cache", zap.Error(err))
	}
	return c.backend.Close()
}

func (c *cachePlugin) loadDump() error {
	if len(c.args.DumpFile) == 0 {
		return nil
	}
	b, err := os.ReadFile(c.args.DumpFile)
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return err
	}
	if err := c.backend.LoadDump(b, unmarshalKey, unmarshalValue); err != nil {
		return err
	}
	c.L().Info("cache dump loaded", zap.Int("entries", c.backend.Len()))
	return nil
}

// startDumpLoop starts a dump loop in another goroutine. It does not block.
func (c *cachePlugin) startDumpLoop() {
	if len(c.args.DumpFile) == 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Duration(c.args.DumpInterval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				keyUpdated := c.updatedKey.Swap(0)
				if keyUpdated < minimumChangesToDump {
					c.updatedKey.Add(keyUpdated)
					continue
				}
				if err := c.dumpCache(); err != nil {
					c.L().Error("dump cache", zap.Error(err))
				}
			case <-c.closeNotify:
				return
			}
		}
	}()
}

func (c *cachePlugin) dumpCache() error {
	if len(c.args.DumpFile) == 0 {
		return nil
	}
	b, n, err := c.backend.Dump(marshalKey, marshalValue)
	if err != nil {
		return err
	}

	f, err := os.Create(c.args.DumpFile)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	c.L().Info("cache dumped", zap.Int("file_size", len(b)), zap.Int("entries", n))
	return nil
}

func (c *cachePlugin) api() *chi.Mux {
	r := chi.NewRouter()
	r.Get("/flush", func(w http.ResponseWriter, req *http.Request) {
		c.backend.Flush()
	})
	r.Get("/dump", func(w http.ResponseWriter, req *http.Request) {
		b, _, err := c.backend.Dump(marshalKey, marshalValue)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/octet-stream")
		_, _ = w.Write(b)
	})
	r.Post("/load_dump", func(w http.ResponseWriter, req *http.Request) {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := c.backend.LoadDump(b, unmarshalKey, unmarshalValue); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return r
}

// shuffle A/AAAA records in m.
func shuffleIP(m *dns.Msg) {
	ans := m.Answer

	// Find out where the a/aaaa records start. Usually is at the suffix.
	ipStart := len(ans) - 1
	for i := len(ans) - 1; i >= 0; i-- {
		switch ans[i].Header().Rrtype {
		case dns.TypeA, dns.TypeAAAA:
			ipStart = i
			continue
		}
		break
	}

	// Shuffle the ip suffix.
	if ipStart >= 0 {
		ips := ans[ipStart:]
		rand.Shuffle(len(ips), func(i, j int) {
			ips[i], ips[j] = ips[j], ips[i]
		})
	}
}

func min[T constraints.Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}
