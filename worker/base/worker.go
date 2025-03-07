package base

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/gc"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/exporter"
	imageexporter "github.com/moby/buildkit/exporter/containerimage"
	localexporter "github.com/moby/buildkit/exporter/local"
	ociexporter "github.com/moby/buildkit/exporter/oci"
	tarexporter "github.com/moby/buildkit/exporter/tar"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/snapshot/imagerefchecker"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/mounts"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/source/containerimage"
	"github.com/moby/buildkit/source/git"
	"github.com/moby/buildkit/source/http"
	"github.com/moby/buildkit/source/local"
	"github.com/moby/buildkit/util/archutil"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/controller"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const labelCreatedAt = "buildkit/createdat"

// TODO: this file should be removed. containerd defines ContainerdWorker, oci defines OCIWorker. There is no base worker.

// WorkerOpt is specific to a worker.
// See also CommonOpt.
type WorkerOpt struct {
	ID              string
	Labels          map[string]string
	Platforms       []ocispecs.Platform
	GCPolicy        []client.PruneInfo
	Executor        executor.Executor
	Snapshotter     snapshot.Snapshotter
	ContentStore    content.Store
	Applier         diff.Applier
	Differ          diff.Comparer
	ImageStore      images.Store // optional
	RegistryHosts   docker.RegistryHosts
	IdentityMapping *idtools.IdentityMapping
	LeaseManager    leases.Manager
	GarbageCollect  func(context.Context) (gc.Stats, error)
	ParallelismSem  *semaphore.Weighted
	MetadataStore   *metadata.Store
	MountPoolRoot   string
}

// Worker is a local worker instance with dedicated snapshotter, cache, and so on.
// TODO: s/Worker/OpWorker/g ?
type Worker struct {
	WorkerOpt
	CacheMgr      cache.Manager
	SourceManager *source.Manager
	imageWriter   *imageexporter.ImageWriter
	ImageSource   *containerimage.Source
}

// NewWorker instantiates a local worker
func NewWorker(ctx context.Context, opt WorkerOpt) (*Worker, error) {
	imageRefChecker := imagerefchecker.New(imagerefchecker.Opt{
		ImageStore:   opt.ImageStore,
		ContentStore: opt.ContentStore,
	})

	cm, err := cache.NewManager(cache.ManagerOpt{
		Snapshotter:     opt.Snapshotter,
		PruneRefChecker: imageRefChecker,
		Applier:         opt.Applier,
		GarbageCollect:  opt.GarbageCollect,
		LeaseManager:    opt.LeaseManager,
		ContentStore:    opt.ContentStore,
		Differ:          opt.Differ,
		MetadataStore:   opt.MetadataStore,
		MountPoolRoot:   opt.MountPoolRoot,
	})
	if err != nil {
		return nil, err
	}

	sm, err := source.NewManager()
	if err != nil {
		return nil, err
	}

	is, err := containerimage.NewSource(containerimage.SourceOpt{
		Snapshotter:   opt.Snapshotter,
		ContentStore:  opt.ContentStore,
		Applier:       opt.Applier,
		ImageStore:    opt.ImageStore,
		CacheAccessor: cm,
		RegistryHosts: opt.RegistryHosts,
		LeaseManager:  opt.LeaseManager,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(is)

	if err := git.Supported(); err == nil {
		gs, err := git.NewSource(git.Opt{
			CacheAccessor: cm,
		})
		if err != nil {
			return nil, err
		}
		sm.Register(gs)
	} else {
		bklog.G(ctx).Warnf("git source cannot be enabled: %v", err)
	}

	hs, err := http.NewSource(http.Opt{
		CacheAccessor: cm,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(hs)

	ss, err := local.NewSource(local.Opt{
		CacheAccessor: cm,
	})
	if err != nil {
		return nil, err
	}
	sm.Register(ss)

	iw, err := imageexporter.NewImageWriter(imageexporter.WriterOpt{
		Snapshotter:  opt.Snapshotter,
		ContentStore: opt.ContentStore,
		Applier:      opt.Applier,
		Differ:       opt.Differ,
	})
	if err != nil {
		return nil, err
	}

	leases, err := opt.LeaseManager.List(ctx, "labels.\"buildkit/lease.temporary\"")
	if err != nil {
		return nil, err
	}
	for _, l := range leases {
		opt.LeaseManager.Delete(ctx, l)
	}

	return &Worker{
		WorkerOpt:     opt,
		CacheMgr:      cm,
		SourceManager: sm,
		imageWriter:   iw,
		ImageSource:   is,
	}, nil
}

func (w *Worker) ContentStore() content.Store {
	return w.WorkerOpt.ContentStore
}

func (w *Worker) ID() string {
	return w.WorkerOpt.ID
}

func (w *Worker) Labels() map[string]string {
	return w.WorkerOpt.Labels
}

func (w *Worker) Platforms(noCache bool) []ocispecs.Platform {
	if noCache {
		for _, p := range archutil.SupportedPlatforms(noCache) {
			exists := false
			for _, pp := range w.WorkerOpt.Platforms {
				if platforms.Only(pp).Match(p) {
					exists = true
					break
				}
			}
			if !exists {
				w.WorkerOpt.Platforms = append(w.WorkerOpt.Platforms, p)
			}
		}
	}
	return w.WorkerOpt.Platforms
}

func (w *Worker) GCPolicy() []client.PruneInfo {
	return w.WorkerOpt.GCPolicy
}

func (w *Worker) LoadRef(ctx context.Context, id string, hidden bool) (cache.ImmutableRef, error) {
	var opts []cache.RefOption
	if hidden {
		opts = append(opts, cache.NoUpdateLastUsed)
	}
	if id == "" {
		// results can have nil refs if they are optimized out to be equal to scratch,
		// i.e. Diff(A,A) == scratch
		return nil, nil
	}

	var pg progress.Controller
	optGetter := solver.CacheOptGetterOf(ctx)
	if optGetter != nil {
		if kv := optGetter(false, cache.ProgressKey{}); kv != nil {
			if v, ok := kv[cache.ProgressKey{}].(progress.Controller); ok {
				pg = v
			}
		}
	}

	ref, err := w.CacheMgr.Get(ctx, id, pg, opts...)
	var needsRemoteProviders cache.NeedsRemoteProviderError
	if errors.As(err, &needsRemoteProviders) {
		if optGetter != nil {
			var keys []interface{}
			for _, dgst := range needsRemoteProviders {
				keys = append(keys, cache.DescHandlerKey(dgst))
			}
			descHandlers := cache.DescHandlers(make(map[digest.Digest]*cache.DescHandler))
			for k, v := range optGetter(true, keys...) {
				if key, ok := k.(cache.DescHandlerKey); ok {
					if handler, ok := v.(*cache.DescHandler); ok {
						descHandlers[digest.Digest(key)] = handler
					}
				}
			}
			opts = append(opts, descHandlers)
			ref, err = w.CacheMgr.Get(ctx, id, pg, opts...)
		}
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to load ref")
	}
	return ref, nil
}

func (w *Worker) Executor() executor.Executor {
	return w.WorkerOpt.Executor
}

func (w *Worker) CacheManager() cache.Manager {
	return w.CacheMgr
}

func (w *Worker) ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager) (solver.Op, error) {
	if baseOp, ok := v.Sys().(*pb.Op); ok {
		switch op := baseOp.Op.(type) {
		case *pb.Op_Source:
			return ops.NewSourceOp(v, op, baseOp.Platform, w.SourceManager, w.ParallelismSem, sm, w)
		case *pb.Op_Exec:
			return ops.NewExecOp(v, op, baseOp.Platform, w.CacheMgr, w.ParallelismSem, sm, w.WorkerOpt.Executor, w)
		case *pb.Op_File:
			return ops.NewFileOp(v, op, w.CacheMgr, w.ParallelismSem, w)
		case *pb.Op_Build:
			return ops.NewBuildOp(v, op, s, w)
		case *pb.Op_Merge:
			return ops.NewMergeOp(v, op, w)
		case *pb.Op_Diff:
			return ops.NewDiffOp(v, op, w)
		default:
			return nil, errors.Errorf("no support for %T", op)
		}
	}
	return nil, errors.Errorf("could not resolve %v", v)
}

func (w *Worker) PruneCacheMounts(ctx context.Context, ids []string) error {
	mu := mounts.CacheMountsLocker()
	mu.Lock()
	defer mu.Unlock()

	for _, id := range ids {
		mds, err := mounts.SearchCacheDir(ctx, w.CacheMgr, id)
		if err != nil {
			return err
		}
		for _, md := range mds {
			if err := md.SetCachePolicyDefault(); err != nil {
				return err
			}
			if err := md.ClearCacheDirIndex(); err != nil {
				return err
			}
			// if ref is unused try to clean it up right away by releasing it
			if mref, err := w.CacheMgr.GetMutable(ctx, md.ID()); err == nil {
				go mref.Release(context.TODO())
			}
		}
	}

	mounts.ClearActiveCacheMounts()
	return nil
}

func (w *Worker) ResolveImageConfig(ctx context.Context, ref string, opt llb.ResolveImageConfigOpt, sm *session.Manager, g session.Group) (digest.Digest, []byte, error) {
	return w.ImageSource.ResolveImageConfig(ctx, ref, opt, sm, g)
}

func (w *Worker) DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error) {
	return w.CacheMgr.DiskUsage(ctx, opt)
}

func (w *Worker) Prune(ctx context.Context, ch chan client.UsageInfo, opt ...client.PruneInfo) error {
	return w.CacheMgr.Prune(ctx, ch, opt...)
}

func (w *Worker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	switch name {
	case client.ExporterImage:
		return imageexporter.New(imageexporter.Opt{
			Images:         w.ImageStore,
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			RegistryHosts:  w.RegistryHosts,
			LeaseManager:   w.LeaseManager,
		})
	case client.ExporterLocal:
		return localexporter.New(localexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterTar:
		return tarexporter.New(tarexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterOCI:
		return ociexporter.New(ociexporter.Opt{
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			Variant:        ociexporter.VariantOCI,
			LeaseManager:   w.LeaseManager,
		})
	case client.ExporterDocker:
		return ociexporter.New(ociexporter.Opt{
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			Variant:        ociexporter.VariantDocker,
			LeaseManager:   w.LeaseManager,
		})
	default:
		return nil, errors.Errorf("exporter %q could not be found", name)
	}
}

func (w *Worker) FromRemote(ctx context.Context, remote *solver.Remote) (ref cache.ImmutableRef, err error) {
	if cd, ok := remote.Provider.(interface {
		CheckDescriptor(context.Context, ocispecs.Descriptor) error
	}); ok && len(remote.Descriptors) > 0 {
		var eg errgroup.Group
		for _, desc := range remote.Descriptors {
			desc := desc
			eg.Go(func() error {
				if err := cd.CheckDescriptor(ctx, desc); err != nil {
					return err
				}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
	}

	descHandler := &cache.DescHandler{
		Provider: func(session.Group) content.Provider { return remote.Provider },
		Progress: &controller.Controller{
			WriterFactory: progress.FromContext(ctx),
		},
	}
	snapshotLabels := func([]ocispecs.Descriptor, int) map[string]string { return nil }
	if cd, ok := remote.Provider.(interface {
		SnapshotLabels([]ocispecs.Descriptor, int) map[string]string
	}); ok {
		snapshotLabels = cd.SnapshotLabels
	}
	descHandlers := cache.DescHandlers(make(map[digest.Digest]*cache.DescHandler))
	for i, desc := range remote.Descriptors {
		descHandlers[desc.Digest] = &cache.DescHandler{
			Provider:       descHandler.Provider,
			Progress:       descHandler.Progress,
			Annotations:    desc.Annotations,
			SnapshotLabels: snapshotLabels(remote.Descriptors, i),
		}
	}

	var current cache.ImmutableRef
	for i, desc := range remote.Descriptors {
		tm := time.Now()
		if tmstr, ok := desc.Annotations[labelCreatedAt]; ok {
			if err := (&tm).UnmarshalText([]byte(tmstr)); err != nil {
				if current != nil {
					current.Release(context.TODO())
				}
				return nil, err
			}
		}
		descr := fmt.Sprintf("imported %s", remote.Descriptors[i].Digest)
		if v, ok := desc.Annotations["buildkit/description"]; ok {
			descr = v
		}
		opts := []cache.RefOption{
			cache.WithDescription(descr),
			cache.WithCreationTime(tm),
			descHandlers,
		}
		if dh, ok := descHandlers[desc.Digest]; ok {
			if ref, ok := dh.Annotations["containerd.io/distribution.source.ref"]; ok {
				opts = append(opts, cache.WithImageRef(ref)) // can set by registry cache importer
			}
		}
		ref, err := w.CacheMgr.GetByBlob(ctx, desc, current, opts...)
		if current != nil {
			current.Release(context.TODO())
		}
		if err != nil {
			return nil, err
		}
		current = ref
	}
	return current, nil
}

// Labels returns default labels
// utility function. could be moved to the constructor logic?
func Labels(executor, snapshotter string) map[string]string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	labels := map[string]string{
		worker.LabelExecutor:    executor,
		worker.LabelSnapshotter: snapshotter,
		worker.LabelHostname:    hostname,
	}
	return labels
}

// ID reads the worker id from the `workerid` file.
// If not exist, it creates a random one,
func ID(root string) (string, error) {
	f := filepath.Join(root, "workerid")
	b, err := ioutil.ReadFile(f)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			id := identity.NewID()
			err := ioutil.WriteFile(f, []byte(id), 0400)
			return id, err
		}
		return "", err
	}
	return string(b), nil
}
