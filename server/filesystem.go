package server

import (
	"errors"
	"go.uber.org/zap"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Error returned when there is a bad path provided to one of the FS calls.
var InvalidPathResolution = errors.New("invalid path resolution")

type Filesystem struct {
	// The root directory where all of the server data is contained. By default
	// this is going to be /srv/daemon-data but can vary depending on the system.
	Root string

	// The server object associated with this Filesystem.
	Server *Server
}

// Returns the root path that contains all of a server's data.
func (fs *Filesystem) Path() string {
	return filepath.Join(fs.Root, fs.Server.Uuid)
}

// Normalizes a directory being passed in to ensure the user is not able to escape
// from their data directory. After normalization if the directory is still within their home
// path it is returned. If they managed to "escape" an error will be returned.
//
// This logic is actually copied over from the SFTP server code. Ideally that eventually
// either gets ported into this application, or is able to make use of this package.
func (fs *Filesystem) SafePath(p string) (string, error) {
	var nonExistentPathResolution string

	// Calling filpath.Clean on the joined directory will resolve it to the absolute path,
	// removing any ../ type of resolution arguments, and leaving us with a direct path link.
	r := filepath.Clean(filepath.Join(fs.Path(), p))

	// At the same time, evaluate the symlink status and determine where this file or folder
	// is truly pointing to.
	p, err := filepath.EvalSymlinks(r)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	} else if os.IsNotExist(err) {
		// The requested directory doesn't exist, so at this point we need to iterate up the
		// path chain until we hit a directory that _does_ exist and can be validated.
		parts := strings.Split(filepath.Dir(r), "/")

		var try string
		// Range over all of the path parts and form directory pathings from the end
		// moving up until we have a valid resolution or we run out of paths to try.
		for k := range parts {
			try = strings.Join(parts[:(len(parts) - k)], "/")

			if !strings.HasPrefix(try, fs.Path()) {
				break
			}

			t, err := filepath.EvalSymlinks(try)
			if err == nil {
				nonExistentPathResolution = t
				break
			}
		}
	}

	// If the new path doesn't start with their root directory there is clearly an escape
	// attempt going on, and we should NOT resolve this path for them.
	if nonExistentPathResolution != "" {
		if !strings.HasPrefix(nonExistentPathResolution, fs.Path()) {
			return "", InvalidPathResolution
		}

		// If the nonExistentPathResoltion variable is not empty then the initial path requested
		// did not exist and we looped through the pathway until we found a match. At this point
		// we've confirmed the first matched pathway exists in the root server directory, so we
		// can go ahead and just return the path that was requested initially.
		return r, nil
	}

	// If the requested directory from EvalSymlinks begins with the server root directory go
	// ahead and return it. If not we'll return an error which will block any further action
	// on the file.
	if strings.HasPrefix(p, fs.Path()) {
		return p, nil
	}

	return "", InvalidPathResolution
}

// Determines if the directory a file is trying to be added to has enough space available
// for the file to be written to.
//
// Because determining the amount of space being used by a server is a taxing operation we
// will load it all up into a cache and pull from that as long as the key is not expired.
func (fs *Filesystem) HasSpaceAvailable() bool {
	var space = fs.Server.Build.DiskSpace

	// If space is -1 or 0 just return true, means they're allowed unlimited.
	if space <= 0 {
		return true
	}

	var size int64
	if x, exists := fs.Server.Cache().Get("disk_used"); exists {
		size = x.(int64)
	}

	// If there is no size its either because there is no data (in which case running this function
	// will have effectively no impact), or there is nothing in the cache, in which case we need to
	// grab the size of their data directory. This is a taxing operation, so we want to store it in
	// the cache once we've gotten it.
	if size == 0 {
		if size, err := fs.DirectorySize("/"); err != nil {
			zap.S().Warnw("failed to determine directory size", zap.String("server", fs.Server.Uuid), zap.Error(err))
		} else {
			fs.Server.Cache().Set("disk_used", size, time.Minute * 5)
		}
	}

	// Determine if their folder size, in bytes, is smaller than the amount of space they've
	// been allocated.
	return (size / 1024.0 / 1024.0) <= space
}

// Determines the directory size of a given location by running parallel tasks to iterate
// through all of the folders. Returns the size in bytes. This can be a fairly taxing operation
// on locations with tons of files, so it is recommended that you cache the output.
func (fs *Filesystem) DirectorySize(dir string) (int64, error) {
	var size int64
	var wg sync.WaitGroup

	cleaned, err := fs.SafePath(dir)
	if err != nil {
		return 0, err
	}

	files, err := ioutil.ReadDir(cleaned)
	if err != nil {
		return 0, err
	}

	// Iterate over all of the files and directories. If it is a file, immediately add its size
	// to the total size being returned. If we're dealing with a directory, call this function
	// on a seperate thread until we have gotten the size of everything nested within the given
	// directory.
	for _, f := range files {
		if f.IsDir() {
			wg.Add(1)

			go func(p string) {
				defer wg.Done()

				s, _ := fs.DirectorySize(p)
				size += s
			}(filepath.Join(cleaned, f.Name()))
		} else {
			size += f.Size()
		}
	}

	wg.Wait()

	return size, nil
}