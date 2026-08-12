package handlers

// newValidateTestHandlers returns a Handlers whose config carries a default task
// model.
//
// validateTaskCreate refuses a task that has neither its own model nor a
// deployment default, because such a task can never run — the dispatcher fails it
// terminally on the first attempt and dead-letters it (#1014). Tests that exercise
// some OTHER field's validation still need the task to be otherwise runnable, so
// they build their Handlers here instead of with a bare &Handlers{}. Tests that
// pin the model gate itself construct their own.
func newValidateTestHandlers() *Handlers {
	return &Handlers{config: Config{DefaultTaskModel: "test/model"}}
}
