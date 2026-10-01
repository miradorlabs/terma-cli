package model

// ModelPrice is a list price in USD per million tokens.
type ModelPrice struct {
	Input, Output, CacheRead, CacheWrite float64
}

// USD prices a token count at list.
func (p ModelPrice) USD(t Tokens) float64 {
	return (float64(t.Input)*p.Input + float64(t.Output)*p.Output +
		float64(t.CacheRead)*p.CacheRead + float64(t.CacheWrite)*p.CacheWrite) / 1e6
}

// ListPrices is the reference table the simulator prices calls with. The values
// are illustrative; only their ratios matter to the evaluation.
var ListPrices = map[string]ModelPrice{
	"claude-opus-5":      {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-5-fast": {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-sonnet-5":    {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"claude-haiku-4-5":   {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	"gpt-5.6":            {Input: 2.5, Output: 15, CacheRead: 0.25},
	"gpt-5.6-mini":       {Input: 0.5, Output: 3, CacheRead: 0.05},
}

// ListPriceUSD prices a call at list. Fast mode has its own Claude price.
func ListPriceUSD(model string, fast bool, t Tokens) float64 {
	key := model
	if fast {
		if p, ok := ListPrices[model+"-fast"]; ok {
			return p.USD(t)
		}
	}
	return ListPrices[key].USD(t)
}
