package termite

import (
	"fmt"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/termite/attr"
	"log"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var _ = log.Println

func (m *Master) MaybeRunInMaster(req *WorkRequest, rep *WorkResponse) bool {
	binary := req.Binary
	_, binary = filepath.Split(binary)

	switch binary {
	case "mkdir":
		return mkdirMaybeMasterRun(m, req, rep)
	case "rm":
		return rmMaybeMasterRun(m, req, rep)
		// TODO - implement mv ?
	}
	return false
}

// Recursively lists names.  Returns children before the parents.
func recurseNames(master *Master, name string) (names []string) {
	a := master.attributes.GetDir(name)

	for n, m := range a.NameModeMap {
		if m&syscall.S_IFDIR != 0 {
			names = append(names, recurseNames(master, filepath.Join(name, n))...)
		} else {
			names = append(names, filepath.Join(name, n))
		}
	}
	if !a.Deletion() {
		names = append(names, name)
	}
	return
}

func rmMaybeMasterRun(master *Master, req *WorkRequest, rep *WorkResponse) bool {
	g := Getopt(req.Argv[1:], nil, nil, true)

	force := g.HasLong("force") || g.HasShort('f')
	delete(g.Long, "force")
	delete(g.Short, 'f')

	recursive := g.HasLong("recursive") || g.HasShort('r') || g.HasShort('R')
	delete(g.Long, "recursive")
	delete(g.Short, 'R')
	delete(g.Short, 'r')

	if g.HasOptions() {
		return false
	}

	log.Println("Running in master:", req.Summary())
	todo := []string{}
	for _, a := range g.Args {
		if a[0] != '/' {
			a = filepath.Join(req.Dir, a)
		}
		a = strings.TrimLeft(filepath.Clean(a), "/")
		todo = append(todo, a)
	}

	fs := attr.FileSet{}
	msgs := []string{}
	status := 0
	now := time.Now()
	if recursive {
		for _, t := range todo {
			parentDir, _ := SplitPath(t)
			parent := master.attributes.Get(parentDir)
			if parent.Deletion() {
				continue
			}

			parent.SetTimes(nil, &now, &now)
			fs.Files = append(fs.Files, parent)
			for _, n := range recurseNames(master, t) {
				fs.Files = append(fs.Files, &attr.FileAttr{
					Path: n,
				})
			}
		}
	} else {
		for _, p := range todo {
			a := master.attributes.GetDir(p)
			switch {
			case a.Deletion():
				if !force {
					msgs = append(msgs, fmt.Sprintf("rm: no such file or directory: %s", p))
					status = 1
				}
			case a.IsDir() && !recursive:
				msgs = append(msgs, fmt.Sprintf("rm: is a directory: %s", p))
				status = 1
			default:
				parentDir, _ := SplitPath(p)
				parentAttr := master.attributes.Get(parentDir)
				parentAttr.SetTimes(nil, &now, &now)
				fs.Files = append(fs.Files, parentAttr, &attr.FileAttr{Path: p})
			}
		}
	}
	master.replay(fs)

	rep.Stderr = strings.Join(msgs, "\n")
	rep.Exit = syscall.WaitStatus(status << 8)
	return true
}

func mkdirMaybeMasterRun(master *Master, req *WorkRequest, rep *WorkResponse) bool {
	g := Getopt(req.Argv[1:], nil, nil, true)

	hasParent := g.HasLong("parent") || g.HasShort('p')

	delete(g.Long, "parent")
	delete(g.Short, 'p')

	if len(g.Short) > 0 || len(g.Long) > 0 {
		return false
	}
	for _, a := range g.Args {
		// mkdir -p a/../b should create both a and b.
		if strings.Contains(a, "..") {
			return false
		}
	}

	log.Println("Running in master:", req.Summary())
	for _, a := range g.Args {
		if a[0] != '/' {
			a = filepath.Join(req.Dir, a)
		}
		a = filepath.Clean(a)
		if hasParent {
			mkdirParentMasterRun(master, a, rep)
		} else {
			mkdirNormalMasterRun(master, a, rep)
		}
	}
	return true
}

// Should receive full path.
func mkdirParentMasterRun(master *Master, arg string, rep *WorkResponse) {
	rootless := strings.TrimLeft(arg, "/")
	components := strings.Split(rootless, "/")

	msgs := []string{}
	parent := master.attributes.Get("")
	for i := range components {
		p := strings.Join(components[:i+1], "/")

		dirAttr := master.attributes.Get(p)
		if dirAttr.Deletion() {
			entry := mkdirEntry(p)
			m := entry.ModTime()
			c := entry.ChangeTime()
			parent.SetTimes(nil, &m, &c)
			fs := attr.FileSet{
				Files: []*attr.FileAttr{parent, entry},
			}
			master.replay(fs)

			parent = entry
		} else if dirAttr.IsDir() {
			parent = dirAttr
		} else {
			msgs = append(msgs, fmt.Sprintf("Not a directory: /%s", p))
			break
		}
	}

	if len(msgs) > 0 {
		rep.Stderr = strings.Join(msgs, "\n")
		rep.Exit = 1 << 8
	}
}

func mkdirEntry(rootless string) *attr.FileAttr {
	now := time.Now()

	a := &attr.FileAttr{
		Path: rootless,
		Attr: &fuse.Attr{
			Mode: syscall.S_IFDIR | 0755,
		},
		NameModeMap: map[string]attr.FileMode{},
	}
	a.SetTimes(&now, &now, &now)
	return a
}

func mkdirNormalMasterRun(master *Master, arg string, rep *WorkResponse) {
	rootless := strings.TrimLeft(arg, "/")
	dir, _ := SplitPath(rootless)
	dirAttr := master.attributes.Get(dir)
	if dirAttr.Deletion() {
		rep.Stderr = fmt.Sprintf("File not found: /%s", dir)
		rep.Exit = syscall.WaitStatus(1 << 8)
		return
	}

	if !dirAttr.IsDir() {
		rep.Stderr = fmt.Sprintf("Is not a directory: /%s", dir)
		rep.Exit = syscall.WaitStatus(1 << 8)
		return
	}

	chAttr := master.attributes.Get(rootless)
	if !chAttr.Deletion() {
		rep.Stderr = fmt.Sprintf("File exists: /%s", rootless)
		rep.Exit = syscall.WaitStatus(1 << 8)
		return
	}
	chAttr = mkdirEntry(rootless)

	fs := attr.FileSet{}

	ct := chAttr.ChangeTime()
	mt := chAttr.ModTime()
	dirAttr.SetTimes(nil, &mt, &ct)
	fs.Files = append(fs.Files, dirAttr, chAttr)
	master.replay(fs)
}
