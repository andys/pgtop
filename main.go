package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andys/pgtop/internal/db"
	"github.com/andys/pgtop/internal/term"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: pgtop <postgres-uri>\n")
		fmt.Fprintf(os.Stderr, "Example: pgtop postgres://user:pass@host:5432/dbname\n")
		os.Exit(1)
	}
	uri := os.Args[1]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to PostgreSQL
	conn, err := db.Connect(ctx, uri)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	// Get server version info
	version, err := db.GetVersion(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get version: %v\n", err)
		os.Exit(1)
	}

	// Set up terminal
	t, err := term.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init terminal: %v\n", err)
		os.Exit(1)
	}
	defer t.Restore()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)

	app := &App{
		conn:        conn,
		term:        t,
		version:     version,
		selectedRow: 0,
		refreshRate: 2 * time.Second,
	}

	go app.inputLoop(cancel)

	ticker := time.NewTicker(app.refreshRate)
	defer ticker.Stop()

	// Initial render
	app.refresh(ctx)
	app.render()

	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				return
			case syscall.SIGWINCH:
				app.term.UpdateSize()
				app.render()
			}
		case <-ticker.C:
			if !app.detailMode {
				app.refresh(ctx)
				app.render()
			}
		}
	}
}

// App holds the application state.
type App struct {
	conn    db.Conn
	term    *term.Term
	version string

	mu          sync.Mutex
	processes   []db.Process
	stats       db.Stats
	selectedRow int
	detailMode  bool
	detailText  string
	refreshRate time.Duration
	prevTotal   int64
	prevTime    time.Time
}

func (a *App) refresh(ctx context.Context) {
	procs, err := db.GetProcesses(ctx, a.conn)
	if err != nil {
		return
	}

	stats, err := db.GetStats(ctx, a.conn)
	if err != nil {
		// Use empty stats on error
		stats = db.Stats{}
	}

	// Compute queries/sec from xact_commit + xact_rollback delta
	now := time.Now()
	newTotal := stats.XactCommit + stats.XactRollback
	var qps float64
	if !a.prevTime.IsZero() && now.Sub(a.prevTime) > 0 {
		dt := now.Sub(a.prevTime).Seconds()
		qps = float64(newTotal-a.prevTotal) / dt
	}

	// Filter out idle processes
	var active []db.Process
	for _, p := range procs {
		if p.State != "idle" && p.State != "" {
			active = append(active, p)
		}
	}

	// Sort by running time descending
	sort.Slice(active, func(i, j int) bool {
		return active[i].QuerySeconds > active[j].QuerySeconds
	})

	a.mu.Lock()
	a.processes = active
	stats.QueriesPerSec = qps
	stats.TotalConns = len(procs)
	stats.ActiveConns = len(active)
	a.stats = stats
	a.prevTotal = newTotal
	a.prevTime = now
	// Clamp selectedRow
	if a.selectedRow >= len(active) {
		a.selectedRow = len(active) - 1
	}
	if a.selectedRow < 0 {
		a.selectedRow = 0
	}
	a.mu.Unlock()
}

func (a *App) render() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.detailMode {
		a.renderDetail()
		return
	}

	w, h := a.term.Size()
	var buf strings.Builder

	// Clear screen, move to top
	buf.WriteString("\033[2J\033[H")

	// === Header lines ===
	// Line 1: pgtop title + version
	line1 := fmt.Sprintf("\033[1;37mpgtop\033[0m — PostgreSQL %s", a.version)
	buf.WriteString(line1)
	buf.WriteString("\n")

	// Line 2: stats
	line2 := fmt.Sprintf(
		"Queries/sec: \033[1;33m%.1f\033[0m  |  Conns: \033[1;36m%d\033[0m total, \033[1;32m%d\033[0m active  |  DB: \033[1;35m%s\033[0m",
		a.stats.QueriesPerSec,
		a.stats.TotalConns,
		a.stats.ActiveConns,
		a.stats.DatabaseName,
	)
	buf.WriteString(line2)
	buf.WriteString("\n")

	// Line 3: more stats
	line3 := fmt.Sprintf(
		"TxCommit: %d  TxRollback: %d  BlksRead: %d  BlksHit: %d  TupReturned: %d  TupFetched: %d",
		a.stats.XactCommit,
		a.stats.XactRollback,
		a.stats.BlksRead,
		a.stats.BlksHit,
		a.stats.TupReturned,
		a.stats.TupFetched,
	)
	buf.WriteString(line3)
	buf.WriteString("\n")

	// Separator
	buf.WriteString(strings.Repeat("─", w))
	buf.WriteString("\n")

	headerLines := 4

	// === Table header ===
	// Columns: [selector 2] [PID 7] [Time 8] [User 12] [Locks 5] [DB 12] [Query ...]
	selW := 2
	pidW := 7
	timeW := 8
	userW := 12
	locksW := 5
	dbW := 20
	fixedW := selW + 1 + pidW + 1 + timeW + 1 + userW + 1 + locksW + 1 + dbW + 1 + 1
	queryW := w - fixedW
	if queryW < 10 {
		queryW = 10
	}

	headerFmt := fmt.Sprintf("\033[1;7m%%-%ds %%-%ds %%-%ds %%-%ds %%-%ds %%-%ds %%-%ds\033[0m\n",
		selW, pidW, timeW, userW, locksW, dbW, queryW)
	buf.WriteString(fmt.Sprintf(headerFmt, "", "PID", "TIME", "USER", "LOCKS", "DATABASE", "QUERY"))

	headerLines++

	// === Table rows ===
	maxRows := h - headerLines - 1
	if maxRows < 0 {
		maxRows = 0
	}

	for i, p := range a.processes {
		if i >= maxRows {
			break
		}

		sel := "  "
		if i == a.selectedRow {
			sel = "\033[1;33m*\033[0m "
		}

		pid := fmt.Sprintf("%-*d", pidW, p.PID)

		// Format time
		timeStr := formatDuration(p.QuerySeconds)
		timeStr = padRight(timeStr, timeW)

		user := padRight(truncate(p.User, userW), userW)
		locks := padRight(fmt.Sprintf("%d", p.Locks), locksW)
		dbname := padRight(truncate(p.Database, dbW), dbW)

		// Query: clean up whitespace, truncate
		query := strings.ReplaceAll(p.Query, "\n", " ")
		query = strings.ReplaceAll(query, "\t", " ")
		query = compactSpaces(query)
		query = truncate(query, queryW)

		row := fmt.Sprintf("%s %s %s %s %s %s %s",
			sel, pid, timeStr, user, locks, dbname, query)
		buf.WriteString(row)
		buf.WriteString("\n")
	}

	// Status bar at bottom
	buf.WriteString(fmt.Sprintf("\033[%d;1H", h))
	statusBar := fmt.Sprintf("\033[7m ↑/↓ select  |  Enter: detail  |  q: quit  |  Refreshing every %s \033[0m", a.refreshRate)
	buf.WriteString(padRight(statusBar, w))

	fmt.Fprint(os.Stdout, buf.String())
}

func (a *App) renderDetail() {
	w, h := a.term.Size()
	var buf strings.Builder

	buf.WriteString("\033[2J\033[H")
	buf.WriteString("\033[1;37m=== Process Detail ===\033[0m\n\n")

	// Write detail text, respecting terminal height
	lines := strings.Split(a.detailText, "\n")
	maxLines := h - 3
	for i, line := range lines {
		if i >= maxLines {
			break
		}
		buf.WriteString(truncate(line, w))
		buf.WriteString("\n")
	}

	buf.WriteString(fmt.Sprintf("\033[%d;1H", h))
	buf.WriteString("\033[7m Press any key to return \033[0m")

	fmt.Fprint(os.Stdout, buf.String())
}

func (a *App) inputLoop(cancel context.CancelFunc) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			continue
		}

		input := buf[:n]

		a.mu.Lock()
		inDetail := a.detailMode
		a.mu.Unlock()

		if inDetail {
			// Any key returns from detail mode
			a.mu.Lock()
			a.detailMode = false
			a.mu.Unlock()
			a.render()
			continue
		}

		// Check for escape sequences (arrow keys)
		if n >= 3 && input[0] == 0x1b && input[1] == '[' {
			switch input[2] {
			case 'A': // Up arrow
				a.mu.Lock()
				if a.selectedRow > 0 {
					a.selectedRow--
				}
				a.mu.Unlock()
				a.render()
			case 'B': // Down arrow
				a.mu.Lock()
				if a.selectedRow < len(a.processes)-1 {
					a.selectedRow++
				}
				a.mu.Unlock()
				a.render()
			}
			continue
		}

		switch input[0] {
		case 'q', 'Q', 3: // q, Q, or Ctrl+C
			cancel()
			return
		case 13: // Enter
			a.showDetail()
		}
	}
}

func (a *App) showDetail() {
	a.mu.Lock()
	if len(a.processes) == 0 || a.selectedRow >= len(a.processes) {
		a.mu.Unlock()
		return
	}
	proc := a.processes[a.selectedRow]
	a.mu.Unlock()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	var sb strings.Builder

	// Full query
	sb.WriteString(fmt.Sprintf("PID: %d  |  User: %s  |  Database: %s  |  State: %s\n",
		proc.PID, proc.User, proc.Database, proc.State))
	sb.WriteString(fmt.Sprintf("Running for: %s  |  Locks: %d\n\n",
		formatDuration(proc.QuerySeconds), proc.Locks))

	sb.WriteString("── Full Query ──\n\n")
	sb.WriteString(proc.Query)
	sb.WriteString("\n")

	// EXPLAIN
	sb.WriteString("\n── EXPLAIN ──\n\n")
	explainOutput, err := db.Explain(ctx, a.conn, proc.Query)
	if err != nil {
		sb.WriteString(fmt.Sprintf("EXPLAIN failed: %v\n", err))
	} else {
		sb.WriteString(explainOutput)
	}

	// Locks
	sb.WriteString("\n── Locks ──\n\n")
	locks, err := db.GetLocks(ctx, a.conn, proc.PID)
	if err != nil {
		sb.WriteString(fmt.Sprintf("Failed to get locks: %v\n", err))
	} else if len(locks) == 0 {
		sb.WriteString("No locks held.\n")
	} else {
		// Header
		sb.WriteString(fmt.Sprintf("%-15s %-10s %-20s %-20s %-25s %-7s\n",
			"DATABASE", "SCHEMA", "TABLE", "INDEX", "MODE", "GRANTED"))
		sb.WriteString(strings.Repeat("-", 100))
		sb.WriteString("\n")
		for _, l := range locks {
			sb.WriteString(fmt.Sprintf("%-15s %-10s %-20s %-20s %-25s %-7s\n",
				l.Database, l.Schema, l.Table, l.Index, l.Mode, boolStr(l.Granted)))
		}
	}

	a.mu.Lock()
	a.detailMode = true
	a.detailText = sb.String()
	a.mu.Unlock()

	a.render()
}

// Helpers

func formatDuration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func padRight(s string, width int) string {
	// Account for ANSI escape codes in length calculation
	visible := stripAnsi(s)
	if len(visible) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(visible))
}

func stripAnsi(s string) string {
	var result strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
			continue
		}
		result.WriteRune(r)
	}
	return result.String()
}

func compactSpaces(s string) string {
	var result strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if !prevSpace {
				result.WriteRune(r)
			}
			prevSpace = true
		} else {
			result.WriteRune(r)
			prevSpace = false
		}
	}
	return result.String()
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
