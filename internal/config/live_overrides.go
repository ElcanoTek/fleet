package config

// ── Admin-managed live feature settings ──
//
// The web admin page's Features panel (internal/settings) lets an admin
// override a curated set of feature toggles at runtime, DB row > env var >
// built-in default. The config-backed ones land here: each gets a Live* getter
// (what consumers read per turn / per run) and a Set* setter (what the
// settings apply hook calls), synchronized on the SAME reload mutex the #286
// env-file hot-reload uses — one lock guards every runtime mutation of Config.
//
// The two mechanisms never fight: the #286 reloadable set (cost/token/
// iteration ceilings, temperatures) and the admin-settings set are disjoint,
// and boot writes happen-before serving, so direct field access stays correct
// during Load and in tests (nil reload state → direct access, same contract as
// reload.go's Live* getters).

// liveBool reads a bool field under the runtime read lock.
func (c *Config) liveBool(read func() bool) bool {
	if c.reload == nil {
		return read()
	}
	c.reload.mu.RLock()
	defer c.reload.mu.RUnlock()
	return read()
}

// liveInt reads an int field under the runtime read lock.
func (c *Config) liveInt(read func() int) int {
	if c.reload == nil {
		return read()
	}
	c.reload.mu.RLock()
	defer c.reload.mu.RUnlock()
	return read()
}

// setLive mutates Config under the runtime write lock. A Config built directly
// in a test (nil reload state) mutates unguarded — such a Config is never
// concurrently served.
func (c *Config) setLive(write func()) {
	if c.reload == nil {
		write()
		return
	}
	c.reload.mu.Lock()
	defer c.reload.mu.Unlock()
	write()
}

// LivePhoneAFriendEnabled reports whether scheduled runs get the one-time
// super-LLM review (#175), admin-override-aware.
func (c *Config) LivePhoneAFriendEnabled() bool {
	return c.liveBool(func() bool { return c.PhoneAFriendEnabled })
}

// SetPhoneAFriendEnabled applies the admin override for phone-a-friend.
func (c *Config) SetPhoneAFriendEnabled(v bool) {
	c.setLive(func() { c.PhoneAFriendEnabled = v })
}

// LiveSubagentsEnabled reports whether the spawn_subagent tool is on
// fleet-wide (#175/#264), admin-override-aware.
func (c *Config) LiveSubagentsEnabled() bool {
	return c.liveBool(func() bool { return c.SubagentsEnabled })
}

// SetSubagentsEnabled applies the admin override for sub-agent delegation.
func (c *Config) SetSubagentsEnabled(v bool) {
	c.setLive(func() { c.SubagentsEnabled = v })
}

// LiveMemoryAutoIndexEnabled reports whether the post-turn memory auto-indexer
// (#234) runs, admin-override-aware.
func (c *Config) LiveMemoryAutoIndexEnabled() bool {
	return c.liveBool(func() bool { return c.MemoryAutoIndexEnabled })
}

// SetMemoryAutoIndexEnabled applies the admin override for memory auto-index.
func (c *Config) SetMemoryAutoIndexEnabled(v bool) {
	c.setLive(func() { c.MemoryAutoIndexEnabled = v })
}

// LiveErrorAnalysisEnabled reports whether failed scheduled tasks get the
// post-failure LLM diagnosis (#317), admin-override-aware.
func (c *Config) LiveErrorAnalysisEnabled() bool {
	return c.liveBool(func() bool { return c.ErrorAnalysisEnabled })
}

// SetErrorAnalysisEnabled applies the admin override for error analysis.
func (c *Config) SetErrorAnalysisEnabled(v bool) {
	c.setLive(func() { c.ErrorAnalysisEnabled = v })
}

// LiveAutoTitle reports whether first turns get an LLM-generated conversation
// title (#302), admin-override-aware.
func (c *Config) LiveAutoTitle() bool {
	return c.liveBool(func() bool { return c.AutoTitle })
}

// SetAutoTitle applies the admin override for auto-titling.
func (c *Config) SetAutoTitle(v bool) {
	c.setLive(func() { c.AutoTitle = v })
}

// LiveConnectorRecommendationsEnabled reports whether the connector
// auto-recommendation endpoint (#512) is on, admin-override-aware.
func (c *Config) LiveConnectorRecommendationsEnabled() bool {
	return c.liveBool(func() bool { return c.ConnectorRecommendationsEnabled })
}

// SetConnectorRecommendationsEnabled applies the admin override for connector
// recommendations.
func (c *Config) SetConnectorRecommendationsEnabled(v bool) {
	c.setLive(func() { c.ConnectorRecommendationsEnabled = v })
}

// LiveApprovalTimeoutSeconds reports the global approval default-deny window
// (#225), admin-override-aware. Read at stager construction — once per turn —
// so an admin edit governs the next staged card without a restart. Non-positive
// values keep their boot semantics: "use the hardcoded default", resolved by
// the stager, never here.
func (c *Config) LiveApprovalTimeoutSeconds() int {
	return c.liveInt(func() int { return c.ApprovalTimeoutSeconds })
}

// SetApprovalTimeoutSeconds applies the admin override for the approval
// default-deny window.
func (c *Config) SetApprovalTimeoutSeconds(v int) {
	c.setLive(func() { c.ApprovalTimeoutSeconds = v })
}

// LiveContextHandlesEnabled reports whether composer context handles (#517)
// are on, admin-override-aware.
func (c *Config) LiveContextHandlesEnabled() bool {
	return c.liveBool(func() bool { return c.ContextHandlesEnabled })
}

// SetContextHandlesEnabled applies the admin override for context handles.
func (c *Config) SetContextHandlesEnabled(v bool) {
	c.setLive(func() { c.ContextHandlesEnabled = v })
}
