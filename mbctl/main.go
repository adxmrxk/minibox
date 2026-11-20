// mbctl lists and cleans up minibox run records and reads live cgroup stats.
//
// Usage:
//
//	mbctl ps      [--dir DIR]   list runs with status, exit code, memory and pids
//	mbctl prune   [--dir DIR]   delete records for runs that have exited or died
//
// DIR defaults to $MINIBOX_STATE_DIR, then /run/minibox.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// record mirrors the JSON that minibox writes for each run.
type record struct {
	ID         uint32   `json:"id"`
	Cmd        []string `json:"cmd"`
	StartedUTC int64    `json:"started_unix"`
	Cgroup     *string  `json:"cgroup"`
	InitPID    *int     `json:"init_pid"`
	IP         *string  `json:"ip"`
	Status     string   `json:"status"`
	ExitCode   *int     `json:"exit_code"`

	path string // file the record was loaded from
}

// view is a record plus what mbctl worked out about it.
type view struct {
	record
	live  string // "running", "exited", "dead" (record says running but the process is gone)
	memMB string
	pids  string
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fsFlags := flagSet(os.Args[2:])
	dir := fsFlags.dir

	switch cmd {
	case "ps":
		if err := ps(dir); err != nil {
			fail(err)
		}
	case "logs":
		if len(os.Args) < 3 {
			usage()
		}
		if err := logs(dir, os.Args[2]); err != nil {
			fail(err)
		}
	case "prune":
		if err := prune(dir); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

type flags struct{ dir string }

func flagSet(args []string) flags {
	f := flags{dir: defaultDir()}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 < len(args) {
				f.dir = args[i+1]
				i++
			}
		default:
			// Plain arguments (such as a run ID for "logs") are handled by the command itself.
			if strings.HasPrefix(args[i], "--") {
				usage()
			}
		}
	}
	return f
}

func defaultDir() string {
	if d := os.Getenv("MINIBOX_STATE_DIR"); d != "" {
		return d
	}
	return "/run/minibox"
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mbctl ps|prune|logs ID [--dir DIR]")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mbctl:", err)
	os.Exit(1)
}

func load(dir string) ([]record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r record
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		r.path = p
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedUTC < out[j].StartedUTC })
	return out, nil
}

func processAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func classify(r record) view {
	v := view{record: r, live: r.Status, memMB: "-", pids: "-"}
	if r.Status == "running" {
		if r.InitPID == nil || !processAlive(*r.InitPID) {
			v.live = "dead"
		} else if r.Cgroup != nil {
			v.memMB = readCgroupMB(*r.Cgroup + "/memory.current")
			v.pids = readCgroupValue(*r.Cgroup + "/pids.current")
		}
	}
	return v
}

func readCgroupValue(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "-"
	}
	return strings.TrimSpace(string(raw))
}

func readCgroupMB(path string) string {
	v := readCgroupValue(path)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(n)/(1024*1024))
}

func ps(dir string) error {
	recs, err := load(dir)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Println("no runs recorded in", dir)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tEXIT\tIP\tMEM(MB)\tPIDS\tAGE\tCOMMAND")
	for _, r := range recs {
		v := classify(r)
		exit := "-"
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		}
		age := time.Since(time.Unix(r.StartedUTC, 0)).Round(time.Second)
		ip := "-"
		if r.IP != nil {
			ip = *r.IP
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, v.live, exit, ip, v.memMB, v.pids, age, strings.Join(r.Cmd, " "))
	}
	return w.Flush()
}

// logs prints the saved stdout and stderr for one run.
func logs(dir, id string) error {
	raw, err := os.ReadFile(filepath.Join(dir, id+".log"))
	if err != nil {
		return fmt.Errorf("no logs for run %s: %w", id, err)
	}
	_, err = os.Stdout.Write(raw)
	return err
}

func prune(dir string) error {
	recs, err := load(dir)
	if err != nil {
		return err
	}
	removed := 0
	for _, r := range recs {
		switch classify(r).live {
		case "exited", "dead":
			if err := os.Remove(r.path); err != nil {
				fmt.Fprintln(os.Stderr, "mbctl: remove", r.path+":", err)
				continue
			}
			removed++
		}
	}
	fmt.Printf("removed %d record(s)\n", removed)
	return nil
}
