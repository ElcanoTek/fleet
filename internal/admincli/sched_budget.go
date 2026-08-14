package admincli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// cmdSchedBudget dispatches `fleet sched budget list|create|delete`.
func cmdSchedBudget(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet sched budget list|create|delete")
	}
	switch argv[0] {
	case "list", "ls":
		return schedBudgetList(argv[1:])
	case "create", "add", "upsert":
		return schedBudgetCreate(argv[1:])
	case "delete", "del", "rm":
		return schedBudgetDelete(argv[1:])
	default:
		return errf(1, "unknown sched budget subcommand %q (want list|create|delete)", argv[0])
	}
}

func schedBudgetList(argv []string) int {
	fs := flag.NewFlagSet("sched budget list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	budgets, err := st.ListBudgets(context.Background())
	if err != nil {
		return errf(5, "list budgets: %v", err)
	}
	if len(budgets) == 0 {
		fmt.Fprintln(os.Stderr, "no budgets configured — add one with: fleet sched budget create --scope user --principal <email> --window month --hard-usd 50")
		return 0
	}
	for _, b := range budgets {
		fmt.Printf("%s\t%s\t%s\t%s\tsoft_usd=%s hard_usd=%s soft_tok=%s hard_tok=%s\n",
			b.ID, b.Scope, b.PrincipalID, b.Window,
			fmtOptFloat(b.SoftUSD), fmtOptFloat(b.HardUSD),
			fmtOptInt(b.SoftTokens), fmtOptInt(b.HardTokens))
	}
	return 0
}

func schedBudgetCreate(argv []string) int {
	fs := flag.NewFlagSet("sched budget create", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	scope := fs.String("scope", "user", "scope: user|key")
	principal := fs.String("principal", "", "principal id (user email or API key id)")
	window := fs.String("window", "month", "window: day|week|month")
	softUSD := fs.Float64("soft-usd", -1, "soft USD alert bound (omit to leave unset)")
	hardUSD := fs.Float64("hard-usd", -1, "hard USD refusal bound (omit to leave unset)")
	softTok := fs.Int64("soft-tokens", -1, "soft token alert bound (omit to leave unset)")
	hardTok := fs.Int64("hard-tokens", -1, "hard token refusal bound (omit to leave unset)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	bc := models.BudgetCreate{
		Scope:       strings.TrimSpace(*scope),
		PrincipalID: strings.TrimSpace(*principal),
		Window:      strings.TrimSpace(*window),
	}
	if *softUSD >= 0 {
		v := *softUSD
		bc.SoftUSD = &v
	}
	if *hardUSD >= 0 {
		v := *hardUSD
		bc.HardUSD = &v
	}
	if *softTok >= 0 {
		v := *softTok
		bc.SoftTokens = &v
	}
	if *hardTok >= 0 {
		v := *hardTok
		bc.HardTokens = &v
	}
	if err := bc.Validate(); err != nil {
		return errf(1, "%v", err)
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	b, err := st.UpsertBudget(context.Background(), bc)
	if err != nil {
		return errf(5, "save budget: %v", err)
	}
	fmt.Printf("saved budget %s (%s %s %s)\n", b.ID, b.Scope, b.PrincipalID, b.Window)
	return 0
}

func schedBudgetDelete(argv []string) int {
	fs := flag.NewFlagSet("sched budget delete", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	idStr, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	id, err := uuid.Parse(strings.TrimSpace(idStr))
	if err != nil {
		return errf(1, "usage: fleet sched budget delete <budget_id>")
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	ok, err := st.DeleteBudget(context.Background(), id)
	if err != nil {
		return errf(5, "delete budget: %v", err)
	}
	if !ok {
		return errf(2, "budget %s not found", id)
	}
	fmt.Printf("deleted budget %s\n", id)
	return 0
}

func fmtOptFloat(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *v)
}

func fmtOptInt(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}
