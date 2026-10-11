package usage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ParseQuotaList reads the table `juicefs quota list` prints and returns the used space per directory in GiB. Directories are named after the home PVC (StorageClass juicefs-homes, pathPattern ${.PVC.name}).
func ParseQuotaList(text string) (map[string]float64, error) {
	out := map[string]float64{}
	pathCol, usedCol := -1, -1
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if pathCol < 0 {
			for i, c := range cells {
				switch strings.ToLower(c) {
				case "path", "path/id": // juicefs 1.4 prints "Path/ID"
					pathCol = i
				case "used":
					usedCol = i
				}
			}
			continue
		}
		if usedCol < 0 || pathCol >= len(cells) || usedCol >= len(cells) {
			continue
		}
		gib, ok := parseSize(cells[usedCol])
		if !ok {
			continue
		}
		out[strings.TrimPrefix(cells[pathCol], "/")] = gib
	}
	if pathCol < 0 || usedCol < 0 {
		return nil, errors.New("no table with the columns Path and Used")
	}
	return out, sc.Err()
}

// parseSize reads sizes as JuiceFS prints them ("0 B", "512 KiB", "1.2 GiB", "3G").
func parseSize(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	units := []struct {
		suffix string
		bytes  float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"PiB", 1 << 50},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40}, {"B", 1}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult, s = u.bytes, strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v * mult / (1 << 30), true
}

// HomeRows turns the used space per home directory into rows with profile "home". A home directory is home-<slug>---<hash>, where hash is the first 8 hex digits of SHA-256 of the JupyterHub user name (KubeSpawner's safe names); people are matched by that hash among those the store has seen. A home whose person is not known (yet) is booked to "unbekannt: <dir>", so its storage still shows up.
func HomeRows(ctx context.Context, st Store, used map[string]float64, project ProjectOf, now time.Time) ([]Row, error) {
	people, err := st.Persons(ctx)
	if err != nil {
		return nil, err
	}
	byHash := map[string]string{}
	for _, p := range people {
		sum := sha256.Sum256([]byte(p))
		byHash[hex.EncodeToString(sum[:])[:8]] = p
	}
	day := Day(now)
	var rows []Row
	for dir, gib := range used {
		if !strings.HasPrefix(dir, "home-") {
			continue
		}
		person := "unbekannt: " + dir
		if i := strings.LastIndex(dir, "---"); i >= 0 {
			if p, ok := byHash[dir[i+3:]]; ok {
				person = p
			}
		}
		proj := ""
		if !strings.HasPrefix(person, "unbekannt") {
			proj = project(ctx, person)
		}
		rows = append(rows, Row{Day: day, Project: proj, Person: person, Profile: "home", HomeGB: gib, UpdatedAt: now})
	}
	return rows, nil
}
