package llm

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ad3002/flylab/internal/storage"
)

// BudgetWindow is the rolling window of the AI budgets (contract v4 section 3).
const BudgetWindow = 24 * time.Hour

// UsageStore persists and reads llm_usage rows (storage.Store implements it).
type UsageStore interface {
	RecordLLMUsage(userID *int64, kind string, costUSD float64, ok bool, at time.Time) error
	LLMUsageSince(since time.Time, userID *int64) ([]storage.LLMUsageEntry, error)
}

// Budget enforces AI_DAILY_BUDGET_USD (global) and AI_USER_DAILY_BUDGET_USD (per account)
// over the rolling 24 h spend recorded in llm_usage.
type Budget struct {
	store     UsageStore
	globalUSD float64
	userUSD   float64
	// Now is the clock (tests move it).
	Now func() time.Time
}

// NewBudget returns the budget; both limits must be positive.
func NewBudget(store UsageStore, globalUSD, userUSD float64) (*Budget, error) {
	if store == nil {
		return nil, errors.New("AI budget needs a usage store")
	}
	if !(globalUSD > 0) || math.IsInf(globalUSD, 0) {
		return nil, fmt.Errorf("AI_DAILY_BUDGET_USD must be > 0 (got %v)", globalUSD)
	}
	if !(userUSD > 0) || math.IsInf(userUSD, 0) {
		return nil, fmt.Errorf("AI_USER_DAILY_BUDGET_USD must be > 0 (got %v)", userUSD)
	}
	return &Budget{store: store, globalUSD: globalUSD, userUSD: userUSD, Now: time.Now}, nil
}

// BudgetError means a budget is used up (HTTP 429 AI_BUDGET_EXHAUSTED).
type BudgetError struct {
	Scope     string // ScopeUser | ScopeGlobal
	SpentUSD  float64
	BudgetUSD float64
	ResetsAt  time.Time
}

func (e *BudgetError) Error() string {
	when := e.ResetsAt.UTC().Format(time.RFC3339)
	if e.Scope == ScopeGlobal {
		return fmt.Sprintf("the server-wide AI budget of $%.2f per 24 hours is used up ($%.2f spent); AI planning and interpretations are available again at %s",
			e.BudgetUSD, e.SpentUSD, when)
	}
	return fmt.Sprintf("your AI budget of $%.2f per 24 hours is used up ($%.2f spent); you can use AI planning and interpretations again at %s",
		e.BudgetUSD, e.SpentUSD, when)
}

// BudgetCheckError means the spend could not be read: the call is refused (HTTP 500
// AI_BUDGET_CHECK_FAILED), never treated as "within budget".
type BudgetCheckError struct{ Err error }

func (e *BudgetCheckError) Error() string {
	return fmt.Sprintf("the AI budget cannot be checked, so no AI call is made: %v", e.Err)
}
func (e *BudgetCheckError) Unwrap() error { return e.Err }

// UsageRecordError means a finished claude call could not be written to llm_usage (HTTP 500
// AI_USAGE_RECORD_FAILED): the budget would undercount, so the request fails visibly.
type UsageRecordError struct {
	Err error
	// CallErr is the call's own failure, if it failed too.
	CallErr error
}

func (e *UsageRecordError) Error() string {
	msg := fmt.Sprintf("the AI call finished but its cost could not be recorded for the AI budget: %v", e.Err)
	if e.CallErr != nil {
		msg += fmt.Sprintf(" (the call itself also failed: %v)", e.CallErr)
	}
	return msg
}
func (e *UsageRecordError) Unwrap() error { return e.Err }

// Usage is the rolling 24 h spend against one budget.
type Usage struct {
	SpentUSD  float64
	BudgetUSD float64
	// ResetsAt: when exhausted, the moment the spend drops below the budget again; otherwise
	// the moment the oldest counted call leaves the window; nil when nothing was spent.
	ResetsAt  *time.Time
	Exhausted bool
}

func (b *Budget) usage(userID *int64, limit float64) (*Usage, error) {
	now := b.Now().UTC()
	entries, err := b.store.LLMUsageSince(now.Add(-BudgetWindow), userID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt.Before(entries[j].CreatedAt) })
	u := &Usage{BudgetUSD: limit}
	for _, e := range entries {
		u.SpentUSD += e.CostUSD
	}
	u.SpentUSD = roundUSD(u.SpentUSD)
	if len(entries) == 0 {
		return u, nil
	}
	if u.SpentUSD >= limit {
		u.Exhausted = true
		remaining := u.SpentUSD
		for _, e := range entries {
			remaining -= e.CostUSD
			if roundUSD(remaining) < limit {
				t := e.CreatedAt.Add(BudgetWindow).UTC()
				u.ResetsAt = &t
				break
			}
		}
		return u, nil
	}
	t := entries[0].CreatedAt.Add(BudgetWindow).UTC()
	u.ResetsAt = &t
	return u, nil
}

func roundUSD(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// UserUsage is the account's rolling 24 h spend (GET /api/v1/me ai_usage).
func (b *Budget) UserUsage(userID int64) (*Usage, error) {
	return b.usage(&userID, b.userUSD)
}

// GlobalUsage is the server's rolling 24 h spend (capabilities.ai_budget_available).
func (b *Budget) GlobalUsage() (*Usage, error) {
	return b.usage(nil, b.globalUSD)
}

// Check returns *BudgetError when the account's or the server's budget is used up (the
// account is checked first), *BudgetCheckError when the spend cannot be read, else nil.
func (b *Budget) Check(userID int64) error {
	for _, c := range []struct {
		scope string
		get   func() (*Usage, error)
	}{
		{ScopeUser, func() (*Usage, error) { return b.UserUsage(userID) }},
		{ScopeGlobal, b.GlobalUsage},
	} {
		u, err := c.get()
		if err != nil {
			return &BudgetCheckError{Err: err}
		}
		if u.Exhausted {
			return &BudgetError{Scope: c.scope, SpentUSD: u.SpentUSD, BudgetUSD: u.BudgetUSD, ResetsAt: *u.ResetsAt}
		}
	}
	return nil
}

// Record stores one finished claude call.
func (b *Budget) Record(userID int64, kind string, costUSD float64, ok bool) error {
	uid := userID
	return b.store.RecordLLMUsage(&uid, kind, costUSD, ok, b.Now().UTC())
}
