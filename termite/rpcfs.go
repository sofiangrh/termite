package termite

import (
	"fmt"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/termite/attr"
	"github.com/hanwen/termite/cba"
	"github.com/hanwen/termite/stats"
	"io"
	"log"
	"net/rpc"
	"sync"
	"time"
)

type RpcFs struct {
	fuse.DefaultFileSystem
	cache  *cba.ContentCache
	client *rpc.Client
	contentClient *cba.Client

	// Roots that we should try to fetch locally.
	localRoots []string
	timings    *stats.TimerStats
	attr       *attr.AttributeCache
	id         string

	// Below code is used to make sure we only fetch each hash
	// once.
	mutex    sync.Mutex
	cond     *sync.Cond
	fetching map[string]bool
}

func NewRpcFs(server *rpc.Client, cache *cba.ContentCache, contentConn io.ReadWriteCloser) *RpcFs {
	me := &RpcFs{
		client: server,
		contentClient: cache.NewClient(contentConn),
		timings: stats.NewTimerStats(),
	}
	
	me.attr = attr.NewAttributeCache(
		func(n string) *attr.FileAttr {
			return me.fetchAttr(n)
		}, nil)
	me.cond = sync.NewCond(&me.mutex)
	me.fetching = map[string]bool{}
	me.cache = cache
	return me
}

func (me *RpcFs) Close() {
	me.client.Close()
	me.contentClient.Close()
}

func (me *RpcFs) FetchHash(a *attr.FileAttr) error {
	e := me.FetchHashOnce(a)
	if e == nil && a.Size < uint64(me.cache.Options.MemMaxSize) {
		me.cache.FaultIn(a.Hash)
	}
	return e
}

func (me *RpcFs) FetchHashOnce(a *attr.FileAttr) error {
	me.mutex.Lock()
	defer me.mutex.Unlock()
	h := a.Hash
	for !me.cache.HasHash(h) && me.fetching[h] {
		me.cond.Wait()
	}
	if me.cache.HasHash(h) {
		return nil
	}
	// TODO - necessary?  The contentClient already serializes.
	me.fetching[h] = true
	me.mutex.Unlock()
	
	log.Printf("Fetching contents for file %s: %x", a.Path, h)
	start := time.Now()
	got, err := me.contentClient.Fetch(a.Hash)
	dt := time.Now().Sub(start)
	me.timings.Log("Cache.FetchFile", dt)
	me.timings.LogN("Cache.FetchFileBytes", int64(a.Size), dt)

	if !got && err == nil {
		log.Fatalf("RpcFs.FetchHashOnce: server did not have hash %x", a.Hash)
	}

	me.mutex.Lock()
	delete(me.fetching, h)
	me.cond.Broadcast()
	return err
}

func (me *RpcFs) Update(req *UpdateRequest, resp *UpdateResponse) error {
	me.updateFiles(req.Files)
	return nil
}

func (me *RpcFs) updateFiles(files []*attr.FileAttr) {
	me.attr.Update(files)
}

func (me *RpcFs) fetchAttr(n string) *attr.FileAttr {
	req := &AttrRequest{
		Name:   n,
		Origin: me.id,
	}
	start := time.Now()
	rep := &AttrResponse{}
	err := me.client.Call("FsServer.GetAttr", req, rep)
	dt := time.Now().Sub(start)
	me.timings.Log("FsServer.GetAttr", dt)
	if err != nil {
		// fatal?
		log.Println("GetAttr error:", err)
		return nil
	}

	var wanted *attr.FileAttr
	for _, attr := range rep.Attrs {
		if attr.Path == n {
			wanted = attr
		}
	}

	// TODO - if we got a deletion, we should refetch the parent.
	return wanted
}

////////////////////////////////////////////////////////////////
// FS API

func (me *RpcFs) String() string {
	return "RpcFs"
}

func (me *RpcFs) OpenDir(name string, context *fuse.Context) (chan fuse.DirEntry, fuse.Status) {
	r := me.attr.GetDir(name)
	if r.Deletion() {
		return nil, fuse.ENOENT
	}
	if !r.IsDir() {
		return nil, fuse.EINVAL
	}

	c := make(chan fuse.DirEntry, len(r.NameModeMap))
	for k, mode := range r.NameModeMap {
		c <- fuse.DirEntry{
			Name: k,
			Mode: uint32(mode),
		}
	}
	close(c)
	return c, fuse.OK
}

type rpcFsFile struct {
	fuse.File
	fuse.Attr
}

func (me *rpcFsFile) GetAttr() (*fuse.Attr, fuse.Status) {
	return &me.Attr, fuse.OK
}

func (me *rpcFsFile) String() string {
	return fmt.Sprintf("rpcFsFile(%s)", me.File.String())
}

func (me *RpcFs) Open(name string, flags uint32, context *fuse.Context) (fuse.File, fuse.Status) {
	if flags&fuse.O_ANYWRITE != 0 {
		return nil, fuse.EPERM
	}
	a := me.attr.Get(name)
	if a == nil {
		return nil, fuse.ENOENT
	}
	if a.Deletion() {
		return nil, fuse.ENOENT
	}

	if err := me.FetchHash(a); err != nil {
		log.Printf("Error fetching contents %v", err)
		return nil, fuse.EIO
	}

	if contents := me.cache.ContentsIfLoaded(a.Hash); contents != nil {
		fa := *a.Attr
		return &fuse.WithFlags{
			File: &rpcFsFile{
				fuse.NewDataFile(contents),
				fa,
			},
			FuseFlags: fuse.FOPEN_KEEP_CACHE,
		}, fuse.OK
	}
	fa := *a.Attr
	return &fuse.WithFlags{
		File: &rpcFsFile{
			&LazyLoopbackFile{Name: me.cache.Path(a.Hash)},
			fa,
		},
		FuseFlags: fuse.FOPEN_KEEP_CACHE,
	}, fuse.OK
}

func (me *RpcFs) Readlink(name string, context *fuse.Context) (string, fuse.Status) {
	a := me.attr.Get(name)
	if a == nil {
		return "", fuse.ENOENT
	}

	if a.Deletion() {
		return "", fuse.ENOENT
	}
	if !a.IsSymlink() {
		return "", fuse.EINVAL
	}

	// TODO - kick off getattr on destination.
	return a.Link, fuse.OK
}

func (me *RpcFs) GetAttr(name string, context *fuse.Context) (*fuse.Attr, fuse.Status) {
	r := me.attr.Get(name)
	if r == nil {
		return nil, fuse.ENOENT
	}
	if r.Hash != "" {
		go me.FetchHash(r)
	}
	a := &fuse.Attr{}
	if !r.Deletion() {
		a = r.Attr
	} else {
		a = nil
	}
	return a, r.Status()
}

func (me *RpcFs) Access(name string, mode uint32, context *fuse.Context) (code fuse.Status) {
	if mode == fuse.F_OK {
		_, code := me.GetAttr(name, context)
		return code
	}
	if mode&fuse.W_OK != 0 {
		return fuse.EACCES
	}
	return fuse.OK
}
