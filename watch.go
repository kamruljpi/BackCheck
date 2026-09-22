package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// watch is built to be run as a background process by a Claude session:
// it prints one line ONLY when something changed, so its output can wake the
// session instead of the session polling. Silence means nothing happened.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	interval := fs.Int("interval", 30, "seconds between liveness checks")
	quiet := fs.Int("quiet", 1200, "report once when nothing has happened for this many seconds")
	fromStart := fs.Bool("from-start", false, "replay the whole event log first")
	fs.Parse(args)
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	wt := cfg.WorktreePath()

	f, err := os.OpenFile(cfg.eventsPath(), os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if !*fromStart {
		_, _ = f.Seek(0, io.SeekEnd)
	}
	rd := bufio.NewReader(f)

	say := func(kind, msg string) {
		fmt.Printf("BACKCHECK %s %-14s %s\n", time.Now().Format("15:04:05"), kind, msg)
	}
	lastHead := gitHead(wt)
	lastAlive := cfg.DriverAlive()
	lastActivity := time.Now()
	quietSaid := false
	say("watching", fmt.Sprintf("driver alive=%v head=%s", lastAlive, short(lastHead)))

	tick := time.NewTicker(time.Duration(*interval) * time.Second)
	defer tick.Stop()
	for {
		// drain new events
		for {
			line, err := rd.ReadString('\n')
			if len(line) > 0 && line[len(line)-1] == '\n' {
				var e Event
				if json.Unmarshal([]byte(line), &e) == nil {
					say(e.Event, fmt.Sprintf("w%d %s", e.Wave, e.Msg))
					lastActivity, quietSaid = time.Now(), false
					if e.Event == "closed" {
						say("done", "build CLOSED — watcher exiting")
						return nil
					}
				}
			} else if len(line) > 0 {
				// partial line: rewind and wait for the rest
				_, _ = f.Seek(-int64(len(line)), io.SeekCurrent)
				rd.Reset(f)
			}
			if err != nil {
				break
			}
		}
		select {
		case <-tick.C:
		case <-time.After(2 * time.Second):
			continue
		}
		if alive := cfg.DriverAlive(); alive != lastAlive {
			if alive {
				say("driver-up", "driver is running")
			} else {
				say("driver-down", "no driver holds the lock — is the keeper running?")
			}
			lastAlive, lastActivity, quietSaid = alive, time.Now(), false
		}
		if h := gitHead(wt); h != lastHead {
			say("head-moved", fmt.Sprintf("%s → %s", short(lastHead), short(h)))
			lastHead, lastActivity, quietSaid = h, time.Now(), false
		}
		if !quietSaid && time.Since(lastActivity) > time.Duration(*quiet)*time.Second {
			st := "?"
			if ctl, err := cfg.LoadControl(); err == nil {
				st = ctl.Status
			}
			say("quiet", fmt.Sprintf("nothing for %s (status %s, driver alive=%v)", time.Since(lastActivity).Round(time.Minute), st, lastAlive))
			quietSaid = true
		}
	}
}
