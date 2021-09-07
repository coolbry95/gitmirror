package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// read mapping yaml from ado to bitbucket
// - name: reponame
//   ado: repo url ssh
//   bitbucket: repo url ssh

type mirror struct {
	repos    []repo
	cacheDir string
}

type repo struct {
	root        string // root on disk cachedir+name
	name        string // name of repo from yaml file
	source      string // source url from yaml file
	destination string // destination url from yaml file
}

type Repos struct {
	Repos []RepoMap `yaml:"repos"`
}

type RepoMap struct {
	Name      string `yaml:"name"`
	ADO       string `yaml:"ado"`
	BitBucket string `yaml:"bb"`
}

var (
	flagCacheDir     = flag.String("cachedir", "", "git cache directory")
	repoMappingsFile = flag.String("repomappingsfile", "repos.yaml", "file with repo  mappings")
	flagMirror       = flag.Bool("mirror", false, "enable mirroring to other repos")
)

func main() {
	log.SetFlags(log.Lshortfile | log.Ldate | log.Ltime)

	log.Println("starting git mirror")

	flag.Parse()

	cacheDir, err := createCacheDir()
	if err != nil {
		log.Fatalf("error create cache dir: %v", err)
	}
	log.Printf("created cacheDir: %s", cacheDir)
	_ = cacheDir

	repoMappingfilebytes, err := os.ReadFile(*repoMappingsFile)
	if err != nil {
		log.Fatalf("error reading repomappingsfile: %s :%v", *repoMappingsFile, err)
	}
	log.Printf("read repoMappingsFile: %s", *repoMappingsFile)

	repos := Repos{}
	err = yaml.Unmarshal(repoMappingfilebytes, &repos)
	if err != nil {
		log.Fatalf("error unmarshaling repomappingsfile: %v", err)
	}
	log.Printf("read repoMappingsFile: %s", *repoMappingsFile)

	re := []repo{}
	_ = re
	for _, r := range repos.Repos {
		newRepo := repo{
			root:        filepath.Join(cacheDir, r.Name),
			name:        r.Name,
			source:      r.ADO,
			destination: r.BitBucket,
		}
		re = append(re, newRepo)
	}

	mirror := &mirror{
		repos:    re,
		cacheDir: cacheDir,
	}

	mirror.initRepos()

	_ = mirror
	// fmt.Printf("%v\n", repos)
}

func createCacheDir() (string, error) {
	if *flagCacheDir == "" {
		dir, err := ioutil.TempDir("", "gitmirror")
		if err != nil {
			return "", err
		}

		return dir, nil
	}

	dirStat, err := os.Stat(*flagCacheDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(*flagCacheDir, 0755); err != nil {
			return "", fmt.Errorf("can't create cachedir: %v", err)
		}
	} else {
		if err != nil {
			return "", fmt.Errorf("problem with the -cachedir: %v", err)
		}
		if !dirStat.IsDir() {
			return "", fmt.Errorf("problem with the -cachedir%q: not a directory", *flagCacheDir)
		}
	}

	return *flagCacheDir, nil
}

func (m *mirror) initRepos() {
	const max = 5

	c := make(chan repo)
	for i := 0; i < max; i++ {
		go func(rc chan repo) {
			for r := range rc {
				canReuse := true

				_, err := os.Stat(filepath.Join(r.root, "FETCH_HEAD"))
				if err != nil {
					canReuse = false
					log.Printf("can't resuse repo: %s", r.root)
				}

				if canReuse {
					log.Printf("trying to resuse repo: %s", r.root)
					fetch(r)
					if err != nil {
						canReuse = false
						log.Printf("failed to resuse repo: %s", r.root)
					}
				}

				if !canReuse {
					os.RemoveAll(r.root)
					clone(r)
				}

				// we want to be able to reuse even if we don't mirror
				fetch(r)

				if *flagMirror {
					addRemote(r)
					push(r)
				}
			}
		}(c)
	}

	for _, r := range m.repos {
		c <- r
	}

	close(c)
}

func clone(r repo) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("cloning repo: %s, root: %s", r.name, r.root)
	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", r.source, r.root)

	if err := cmd.Start(); err != nil {
		log.Printf("error starting git clone on repo: %s, err: %v", r.name, err)
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("error waiting git clone on repo: %s, err: %v", r.name, err)
	}
}

func addRemote(r repo) {
	if err := os.MkdirAll(filepath.Join(r.root, "remotes"), 0777); err != nil {
		return
	}

	// We want to include only the refs/heads/* and refs/tags/* namespaces
	// in the mirrors. They correspond to published branches and tags.
	remote := "URL: " + r.destination + "\n" +
		"Push: +refs/heads/*:refs/heads/*\n" +
		"Push: +refs/tags/*:refs/tags/*\n"

	nameAt := strings.Split(r.destination, "@")
	name := strings.Split(nameAt[1], ":")[0]

	ioutil.WriteFile(filepath.Join(r.root, "remotes", name), []byte(remote), 0777)
}

func fetch(r repo) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("fetching repo: %s, root: %s", r.name, r.root)
	cmd := exec.CommandContext(ctx, "git", "fetch", "--prune", "origin")
	cmd.Dir = r.root

	if err := cmd.Start(); err != nil {
		log.Printf("error starting git fetch on repo: %s, err: %v", r.name, err)
		return err
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("error waiting git fetch on repo: %s, err: %v", r.name, err)
		return err
	}

	return nil
}

func push(r repo) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nameAt := strings.Split(r.destination, "@")
	name := strings.Split(nameAt[1], ":")[0]

	log.Printf("pushing repo: %s, root: %s", r.name, r.root)
	cmd := exec.CommandContext(ctx, "git", "push", "--mirror", "--force", name)
	cmd.Dir = r.root

	if err := cmd.Start(); err != nil {
		log.Printf("error starting git push on repo: %s, err: %v", r.name, err)
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("error waiting git push on repo: %s, err: %v", r.name, err)
	}
}
