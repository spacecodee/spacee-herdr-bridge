package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"strings"
	"sync"
	"time"
)

const (
	pairAlphabet   = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no 0/O, 1/I/L
	pairLen        = 6
	pairTTL        = 5 * time.Minute
	maxLiveCodes   = 5
	maxCodeFails   = 10
	codeFailWindow = 10 * time.Minute
	codeLockout    = 10 * time.Minute
)

type redeemResult int

const (
	redeemOK redeemResult = iota
	redeemInvalid
	redeemLocked
)

type pairCode struct {
	code string
	exp  time.Time
}

// pairing holds single-use login codes in memory only.
type pairing struct {
	mu     sync.Mutex
	codes  []pairCode // oldest first
	fails  []time.Time
	lockTo time.Time
}

func (p *pairing) prune(now time.Time) {
	live := p.codes[:0]
	for _, c := range p.codes {
		if now.Before(c.exp) {
			live = append(live, c)
		}
	}
	p.codes = live
}

// issue creates a code valid for pairTTL, dropping the oldest beyond maxLiveCodes.
func (p *pairing) issue(now time.Time) (string, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	var code string
	for {
		var err error
		if code, err = randomCode(); err != nil {
			return "", time.Time{}, err
		}
		if !p.has(code) {
			break
		}
	}
	exp := now.Add(pairTTL)
	p.codes = append(p.codes, pairCode{code, exp})
	if len(p.codes) > maxLiveCodes {
		p.codes = p.codes[len(p.codes)-maxLiveCodes:]
	}
	return code, exp, nil
}

func (p *pairing) has(code string) bool {
	for _, c := range p.codes {
		if c.code == code {
			return true
		}
	}
	return false
}

// redeem consumes input (case-insensitive, spaces ignored). After maxCodeFails
// failures within codeFailWindow every live code is dropped and redeeming is
// locked for codeLockout.
func (p *pairing) redeem(now time.Time, input string) redeemResult {
	input = strings.ToUpper(strings.ReplaceAll(input, " ", ""))
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.lockTo) {
		return redeemLocked
	}
	p.prune(now)
	match := -1
	for i, c := range p.codes { // no early exit: every code is compared
		if subtle.ConstantTimeCompare([]byte(c.code), []byte(input)) == 1 {
			match = i
		}
	}
	if match >= 0 {
		p.codes = append(p.codes[:match], p.codes[match+1:]...)
		return redeemOK
	}
	recent := p.fails[:0]
	for _, t := range p.fails {
		if now.Sub(t) < codeFailWindow {
			recent = append(recent, t)
		}
	}
	p.fails = append(recent, now)
	if len(p.fails) >= maxCodeFails {
		p.codes, p.fails = nil, nil
		p.lockTo = now.Add(codeLockout)
	}
	return redeemInvalid
}

// randomCode draws pairLen characters without modulo bias (31*8 = 248).
func randomCode() (string, error) {
	out := make([]byte, 0, pairLen)
	buf := make([]byte, 16)
	for len(out) < pairLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 248 && len(out) < pairLen {
				out = append(out, pairAlphabet[int(b)%len(pairAlphabet)])
			}
		}
	}
	return string(out), nil
}
