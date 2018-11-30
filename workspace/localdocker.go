package workspace

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
	"github.com/skeema/tengo"
)

// LocalDocker is a Workspace created inside of a Docker container on localhost.
// The schema is dropped when done interacting with the workspace in Cleanup(),
// but the container remains running. The container may optionally be stopped
// or destroyed via Shutdown().
type LocalDocker struct {
	schemaName    string
	d             *tengo.DockerizedInstance
	releaseLock   releaseFunc
	cleanupAction CleanupAction
}

var cstore struct {
	dockerClient *tengo.DockerClient
	containers   map[string]*LocalDocker
	sync.Mutex
}

// NewLocalDocker finds or creates a containerized MySQL instance, creates a
// temporary schema on it, and returns it.
func NewLocalDocker(opts Options) (ld *LocalDocker, err error) {
	if !opts.Flavor.Supported() {
		return nil, fmt.Errorf("NewLocalDocker: unsupported flavor %s", opts.Flavor)
	}

	cstore.Lock()
	defer cstore.Unlock()
	if cstore.dockerClient == nil {
		if cstore.dockerClient, err = tengo.NewDockerClient(tengo.DockerClientOptions{}); err != nil {
			return
		}
		cstore.containers = make(map[string]*LocalDocker)
		tengo.UseFilteredDriverLogger()
	}

	ld = &LocalDocker{
		schemaName:    opts.SchemaName,
		cleanupAction: opts.CleanupAction,
	}
	image := opts.Flavor.String()
	if opts.ContainerName == "" {
		opts.ContainerName = fmt.Sprintf("skeema-%s", strings.Replace(image, ":", "-", -1))
	}
	if cstore.containers[opts.ContainerName] == nil {
		log.Infof("Using container %s (image=%s) for workspace operations", opts.ContainerName, image)
	}
	ld.d, err = cstore.dockerClient.GetOrCreateInstance(tengo.DockerizedInstanceOptions{
		Name:              opts.ContainerName,
		Image:             image,
		RootPassword:      opts.RootPassword,
		DefaultConnParams: opts.DefaultConnParams,
	})
	if err != nil {
		return nil, err
	}

	lockName := fmt.Sprintf("skeema.%s", ld.schemaName)
	if ld.releaseLock, err = getLock(ld.d.Instance, lockName, opts.LockWaitTimeout); err != nil {
		return nil, fmt.Errorf("Unable to obtain lock on %s: %s", ld.d.Instance, err)
	}
	// If this function errors, don't continue to hold the lock
	defer func() {
		if err != nil {
			ld.releaseLock()
			ld = nil
		}
	}()

	if cstore.containers[opts.ContainerName] == nil {
		cstore.containers[opts.ContainerName] = ld
		RegisterShutdownFunc(ld.shutdown)
	}

	if has, err := ld.d.HasSchema(ld.schemaName); err != nil {
		return ld, fmt.Errorf("Unable to check for existence of temp schema on %s: %s", ld.d.Instance, err)
	} else if has {
		// Attempt to drop any tables already present in schema, but fail if any
		// of them actually have 1 or more rows
		if err := ld.d.DropTablesInSchema(ld.schemaName, true); err != nil {
			return ld, fmt.Errorf("Cannot drop existing temporary schema tables on %s: %s", ld.d.Instance, err)
		}
	} else {
		_, err = ld.d.CreateSchema(ld.schemaName, opts.DefaultCharacterSet, opts.DefaultCollation)
		if err != nil {
			return ld, fmt.Errorf("Cannot create temporary schema on %s: %s", ld.d.Instance, err)
		}
	}
	return ld, nil
}

// ConnectionPool returns a connection pool (*sqlx.DB) to the temporary
// workspace schema, using the supplied connection params (which may be blank).
func (ld *LocalDocker) ConnectionPool(params string) (*sqlx.DB, error) {
	return ld.d.Connect(ld.schemaName, params)
}

// IntrospectSchema introspects and returns the temporary workspace schema.
func (ld *LocalDocker) IntrospectSchema() (*tengo.Schema, error) {
	return ld.d.Schema(ld.schemaName)
}

// Cleanup drops the temporary schema from the Dockerized instance. If any
// tables have any rows in the temp schema, the cleanup aborts and an error is
// returned.
// Cleanup does not handle stopping or destroying the container. If requested,
// that is handled by Shutdown() instead, so that containers aren't needlessly
// created and stopped/destroyed multiple times during a program's execution.
func (ld *LocalDocker) Cleanup() error {
	if ld.releaseLock == nil {
		return errors.New("Cleanup() called multiple times on same LocalDocker")
	}
	defer func() {
		ld.releaseLock()
		ld.releaseLock = nil
	}()

	if err := ld.d.DropSchema(ld.schemaName, true); err != nil {
		return fmt.Errorf("Cannot drop temporary schema on %s: %s", ld.d.Instance, err)
	}
	return nil
}

// shutdown handles shutdown logic for a specific LocalDocker instance. A single
// string arg may optionally be supplied as a container name prefix: if the
// container name does not begin with the prefix, no shutdown occurs.
func (ld *LocalDocker) shutdown(args ...interface{}) bool {
	if len(args) > 0 {
		if prefix, ok := args[0].(string); !ok || !strings.HasPrefix(ld.d.Name, prefix) {
			return false
		}
	}

	cstore.Lock()
	defer cstore.Unlock()

	if ld.cleanupAction == CleanupActionStop {
		log.Infof("Stopping container %s", ld.d.Name)
		ld.d.Stop()
	} else if ld.cleanupAction == CleanupActionDestroy {
		log.Infof("Destroying container %s", ld.d.Name)
		ld.d.Destroy()
	}
	delete(cstore.containers, ld.d.Name)
	return true
}
