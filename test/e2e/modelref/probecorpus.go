package main

// The tokenizer corpus: a few fixed texts of the kinds a coding agent sends, counted on every
// probed model. Two models that count each the same share a tokenizer, and a model whose counts
// change has a new one. The texts never change: a change here would read as every model's.
var corpus = []struct{ name, text string }{
	{"prose", `The release notes say the build now retries a request that timed out, waits a little
longer each time, and gives up after the fourth try. Nothing else changed for people who use
it day to day, although the settings page moved two options into an "Advanced" section that
starts collapsed. If you relied on the old layout in a script, check the new field names first.`},
	{"code", `func retry(ctx context.Context, tries int, wait time.Duration, f func() error) error {
	var err error
	for i := 0; i < tries; i++ {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait << i):
		}
	}
	return fmt.Errorf("gave up after %d tries: %w", tries, err)
}`},
	{"json", `{"id":"evt_01","type":"tool_result","tool":"bash","input":{"command":"go test ./...","timeout_ms":120000},"output":{"exit_code":1,"stdout":"--- FAIL: TestRetry (0.02s)\n    retry_test.go:41: got 3 tries, want 4\nFAIL\n","truncated":false},"tags":["ci","flaky"],"elapsed_ms":2104}`},
	{"cjk", `このビルドでは、タイムアウトしたリクエストを再試行するようになりました。设置页面把两个选项移到了“高级”部分。설정 페이지의 두 옵션이 고급 섹션으로 이동했습니다.`},
	{"symbols", `→ ✓ ✗ ⚠ ▸ ◆ ★ ≤ ≥ ≠ ∞ ± × ÷ ° € £ ¥ © ® ™ 🚀 🐛 🔧 ✨ 📦 🧪 ✅ ❌ — – … « » ¿ ¡ § ¶ † ‡`},
	{"indented", "    if err != nil {\n        return err\n    }\n\t\t\t// three tabs\n\t\t\tx := []int{\n\t\t\t\t1,\n\t\t\t\t2,\n\t\t\t}\n\n\n\n        trailing    spaces    \n"},
}
