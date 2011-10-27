package termite

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A in-memory cache of attributes.
//
// Invariants: for all entries, we have their parent directories too
type AttributeCache struct {
	mutex      sync.RWMutex
	attributes map[string]*FileAttr
	cond       *sync.Cond
	busy       map[string]bool
	getter     func(name string) *FileAttr
	statter    func(name string) *os.FileInfo

	clients map[string]*attrCachePending

	Paranoia bool
}

type attrCachePending struct {
	client  AttributeCacheClient
	pending []*FileAttr
	busy    bool
}

type AttributeCacheClient interface {
	Id() string
	Send([]*FileAttr) os.Error
}

func (me *AttributeCache) RmClient(client AttributeCacheClient) {
	id := client.Id()
	me.mutex.Lock()
	defer me.mutex.Unlock()

	c := me.clients[id]
	if c != nil {
		c.pending = nil
		c.busy = false
		me.clients[id] = nil, false
		me.cond.Broadcast()
	}
}

func (me *AttributeCache) AddClient(client AttributeCacheClient) {
	id := client.Id()
	me.mutex.Lock()
	defer me.mutex.Unlock()
	_, ok := me.clients[id]
	if ok {
		log.Panicf("Already have client %q", id)
	}

	clData := attrCachePending{
		client:  client,
		pending: me.copyFiles().Files,
	}

	me.clients[id] = &clData
}

func (me *AttributeCache) Send(client AttributeCacheClient) os.Error {
	me.mutex.Lock()
	defer me.mutex.Unlock()
	id := client.Id()
	c := me.clients[id]
	if c == nil {
		return fmt.Errorf("client %q disappeared", id)
	}

	for c.busy {
		me.cond.Wait()
	}

	if len(c.pending) == 0 {
		return nil
	}
	p := c.pending
	c.pending = nil
	c.busy = true
	me.mutex.Unlock()
	err := c.client.Send(p)
	me.mutex.Lock()
	c.busy = false
	me.cond.Broadcast()
	return err
}

func (me *AttributeCache) Queue(fs FileSet) {
	me.mutex.Lock()
	defer me.mutex.Unlock()
	for _, w := range me.clients {
		w.pending = append(w.pending, fs.Files...)
	}
}

func NewAttributeCache(getter func(n string) *FileAttr,
	statter func(n string) *os.FileInfo) *AttributeCache {
	me := &AttributeCache{
		attributes: make(map[string]*FileAttr),
		busy:       map[string]bool{},
	}
	me.cond = sync.NewCond(&me.mutex)
	me.getter = getter
	me.statter = statter
	me.clients = make(map[string]*attrCachePending)
	return me
}

func (me *AttributeCache) Verify() {
	if !me.Paranoia {
		return
	}
	me.mutex.RLock()
	defer me.mutex.RUnlock()
	me.verify()
}

func (me *AttributeCache) verify() {
	if !me.Paranoia {
		return
	}
	for k, v := range me.attributes {
		if k != "" && filepath.Clean(k) != k {
			log.Panicf("Unclean path %q", k)
		}
		if v.Path != k {
			log.Panicf("attributes mismatch %q %#v", k, v)
		}
		if _, ok := me.busy[k]; ok {
			log.Panicf("busy and attributes entry for %q", k)
		}
		if v.Deletion() {
			log.Panicf("Attribute cache may not contain deletions %q", k)
		}
		if v.IsDirectory() && v.NameModeMap == nil {
			log.Panicf("dir has no NameModeMap %q", k)
		}
		for childName, mode := range v.NameModeMap {
			if strings.Contains(childName, "\000") || strings.Contains(childName, "/") || len(childName) == 0 {
				log.Panicf("%q has illegal child name %q: %o", k, childName, mode)
			}
			if mode == 0 {
				log.Panicf("child has 0 mode: %q.%q", k, childName)
			}
		}
		dir, base := SplitPath(k)
		if base != k {
			parent := me.attributes[dir]
			if v.Deletion() && parent != nil && parent.NameModeMap[base] != 0 {
				log.Panicf("Parent %q has entry for deleted %q", dir, base)
			}
			if !v.Deletion() && parent == nil {
				log.Panicf("Missing parent for %q", k)
			}
			if !v.Deletion() && parent.NameModeMap[base] == 0 {
				log.Panicf("Parent %q has no entry for %q", dir, base)
			}
		}
	}
}

func (me *AttributeCache) Have(name string) bool {
	me.mutex.RLock()
	defer me.mutex.RUnlock()
	_, ok := me.attributes[name]
	return ok
}

func (me *AttributeCache) Get(name string) (rep *FileAttr) {
	return me.get(name, false)
}

func (me *AttributeCache) GetDir(name string) (rep *FileAttr) {
	return me.get(name, true)
}

func (me *AttributeCache) localGet(name string, withdir bool) (rep *FileAttr) {
	me.mutex.RLock()
	defer me.mutex.RUnlock()

	rep, ok := me.attributes[name]
	if ok {
		return rep.Copy(withdir)
	}

	if name != "" {
		dir, base := SplitPath(name)
		dirAttr := me.attributes[dir]
		if dirAttr != nil && dirAttr.NameModeMap != nil && dirAttr.NameModeMap[base] == 0 {
			return &FileAttr{Path: name}
		}
	}
	return nil
}

func (me *AttributeCache) get(name string, withdir bool) (rep *FileAttr) {
	rep = me.localGet(name, withdir)

	if rep != nil {
		return rep
	}

	me.mutex.Lock()
	defer me.mutex.Unlock()
	return me.unsafeGet(name, withdir)
}

func (me *AttributeCache) unsafeGet(name string, withdir bool) (rep *FileAttr) {
	if name != "" {
		dir, base := SplitPath(name)
		dirAttr := me.unsafeGet(dir, true)
		if dirAttr.Deletion() || !dirAttr.IsDirectory() || dirAttr.NameModeMap[base] == 0 {
			return &FileAttr{Path: name}
		}
	}

	for me.busy[name] && me.attributes[name] == nil {
		me.cond.Wait()
	}
	rep, ok := me.attributes[name]
	if ok {
		return rep
	}
	me.busy[name] = true
	me.mutex.Unlock()

	rep = me.getter(name)

	me.mutex.Lock()
	if rep == nil {
		// This is an error, but what can we do?
		return &FileAttr{Path: name}
	}
	rep.Path = name

	if !rep.Deletion() {
		me.attributes[name] = rep
	}
	me.cond.Broadcast()
	me.busy[name] = false, false
	me.verify()
	return rep.Copy(withdir)
}

func (me *AttributeCache) Update(files []*FileAttr) {
	me.mutex.Lock()
	defer me.mutex.Unlock()
	me.update(files)
}

func (me *AttributeCache) update(files []*FileAttr) {
	defer me.verify()
	attributes := me.attributes
	for _, inF := range files {
		r := *inF
		if len(r.Path) > 0 && r.Path[0] == '/' {
			panic("Leading slash.")
		}

		dir, basename := SplitPath(r.Path)
		if basename != "" {
			dirAttr := attributes[dir]
			if dirAttr == nil {
				log.Println("Discarding update: ", r)
				continue
			}
			if dirAttr.NameModeMap == nil {
				log.Panicf("parent dir has no NameModeMap: %q", dir)
			}
			if r.Deletion() {
				dirAttr.NameModeMap[basename] = 0, false
			} else {
				dirAttr.NameModeMap[basename] = FileMode(r.Mode &^ 07777)
			}
		}

		if r.Deletion() {
			attributes[r.Path] = nil, false
			continue
		}

		old := attributes[r.Path]
		if old == nil && r.IsDirectory() && r.NameModeMap == nil {
			// This is a metadata update only. If it does
			// not come with contents, we can't use it to
			// short-cut deletion queries.
			log.Println("Discarding contentless dir update: ", r)
			continue
		}

		if old == nil {
			attributes[r.Path] = &r
		} else {
			old.Merge(r)
		}
		me.busy[r.Path] = false, false
	}
	me.cond.Broadcast()
}

func (me *AttributeCache) Refresh(prefix string) FileSet {
	me.mutex.Lock()
	defer me.mutex.Unlock()

	if prefix != "" && prefix[0] == '/' {
		panic("leading /")
	}

	updated := []*FileAttr{}
	for key, attr := range me.attributes {
		// TODO -should just do everything?
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		fi := me.statter(key)
		if fi == nil && !attr.Deletion() {
			del := FileAttr{
				Path: key,
			}
			updated = append(updated, &del)
		}

		// TODO - should generate deletion based on dir
		// contents too?

		// TODO - does this handle symlinks corrrectly?
		if fi != nil && attr.FileInfo != nil && EncodeFileInfo(*attr.FileInfo) != EncodeFileInfo(*fi) {
			newEnt := me.getter(key)
			newEnt.Path = key
			updated = append(updated, newEnt)
		}
	}
	fs := FileSet{Files: updated}
	fs.Sort()

	me.update(fs.Files)
	return fs
}

func (me *AttributeCache) Copy() FileSet {
	me.mutex.RLock()
	defer me.mutex.RUnlock()

	return me.copyFiles()
}

func (me *AttributeCache) copyFiles() FileSet {
	dump := []*FileAttr{}
	for _, attr := range me.attributes {
		dump = append(dump, attr.Copy(true))
	}

	fs := FileSet{Files: dump}
	fs.Sort()
	return fs
}
