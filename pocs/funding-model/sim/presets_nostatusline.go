package sim

// Variants without a status line: no status line installed, hooks disabled, or
// a headless session. They measure what the allowance view and reconciliation
// do on their own, so the value of the status line is a number, not a claim.
func init() {
	Presets = append(Presets,
		base("team-credits-no-statusline",
			noQuota(claudeSub("ann", "team_tier_1", true, 2, 20)),
			noQuota(func() Persona { p := claudeSub("bo", "team_tier_1", true, 2, 90); p.TokenScale = 2.5; return p }()),
			noQuota(func() Persona { p := claudeSub("cy", "team_tier_2", true, 3, 120); p.TokenScale = 3; return p }()),
		),
		base("hidden-surface-no-statusline",
			noQuota(func() Persona {
				p := claudeSub("ann", "team_tier_1", true, 2, 40)
				p.OtherSurfaceUSDPerHour = 0.2
				return p
			}()),
			noQuota(func() Persona {
				p := claudeSub("bo", "team_tier_1", true, 2, 40)
				p.OtherSurfaceUSDPerHour = 0.07
				return p
			}()),
		),
	)
}
