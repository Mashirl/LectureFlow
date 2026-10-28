// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package dirperm

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// mkdirPrivate creates dir with mode 0700 (the umask can only take bits
// away).
func mkdirPrivate(dir string) error { return os.Mkdir(dir, 0o700) }

// mkdirTempPrivate: os.MkdirTemp already creates 0700 directories.
func mkdirTempPrivate(parent, pattern string) (string, error) { return os.MkdirTemp(parent, pattern) }

func restrict(dir string) error { return os.Chmod(dir, 0o700) }

func save(dir string) (func() error, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	mode := fi.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetgid | os.ModeSetuid)
	return func() error { return os.Chmod(dir, mode) }, nil
}

// node is one directory on the way from / to the checked one.
type node struct {
	path  string
	mode  os.FileMode
	uid   uint32
	group string // the group's name (or number)
}

func check(dir string) (Exposure, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Exposure{}, err
	}
	if real, err = filepath.Abs(real); err != nil {
		return Exposure{}, err
	}
	var chain []node
	for p := real; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return Exposure{}, err
		}
		n := node{path: p, mode: fi.Mode()}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			n.uid = st.Uid
			n.group = strconv.Itoa(int(st.Gid))
			if gr, err := user.LookupGroupId(n.group); err == nil {
				n.group = gr.Name
			}
		} else {
			n.uid = uint32(os.Geteuid())
		}
		chain = append([]node{n}, chain...)
		if filepath.Dir(p) == p {
			break
		}
	}
	me := uint32(os.Geteuid())
	myName := ""
	if u, err := user.LookupId(strconv.Itoa(int(me))); err == nil {
		myName = u.Username
	}
	return exposureOf(chain, me, myName), nil
}

// exposureOf walks from / down to the last node: another account reaches
// it only through directories it may traverse; on the way, a directory it
// may write without the sticky bit (or owns) lets it rename what comes
// next; the last one counts when its group or others may write (read) it,
// or another account owns it. A group named like the user (a "user private
// group") is not another account; root is not another account.
func exposureOf(chain []node, me uint32, myName string) Exposure {
	var e Exposure
	foreign := func(uid uint32) bool { return uid != me && uid != 0 }
	others := func(n node) (bits os.FileMode, who []string) {
		perm := n.mode.Perm()
		if g := (perm >> 3) & 0o7; g != 0 && (myName == "" || n.group != myName) {
			bits |= g
			who = append(who, "group "+n.group)
		}
		if o := perm & 0o7; o != 0 {
			bits |= o
			who = append(who, "others")
		}
		return bits, who
	}
	reach := true
	for i, n := range chain {
		bits, who := others(n)
		if i < len(chain)-1 {
			if reach && foreign(n.uid) {
				e.Write = true
				e.add(fmt.Sprintf("owner %d of %s", n.uid, n.path))
			}
			if reach && bits&0o2 != 0 && n.mode&os.ModeSticky == 0 {
				e.Write = true
				e.add(n.path)
			}
			if bits&0o1 == 0 {
				reach = false
			}
			continue
		}
		if !reach {
			break
		}
		if foreign(n.uid) {
			e.Write = true
			e.add(fmt.Sprintf("owner %d", n.uid))
		}
		if bits&0o2 != 0 {
			e.Write = true
			for _, w := range who {
				e.add(w)
			}
		}
		if bits&0o4 != 0 {
			e.Read = true
			for _, w := range who {
				e.add(w)
			}
		}
	}
	return e
}
