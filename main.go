package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bazelbuild/buildtools/build"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

var (
	debug = os.Getenv("PBSYNC_DEBUG") == "1"
)

const (
	goProtoLibrary = "go_proto_library"
	tsProtoLibrary = "ts_proto_library"
)

var (
	githubRepoRe = regexp.MustCompile(`^github.com/(.+?)/(.+?)/`)
)

func debugf(format string, args ...any) {
	if debug {
		fmt.Fprintf(os.Stderr, "\x1b[33mDEBUG:\x1b[m "+format+"\n", args...)
	}
}

func getBazelBinDir(workspaceRoot string) (string, error) {
	// If the bazel-bin symlink exists, trust it.
	symlink := filepath.Join(workspaceRoot, "bazel-bin")
	target, err := os.Readlink(symlink)
	if err == nil {
		return target, nil
	}

	// Fall back to bazel info (takes ~50ms if bazel is running)
	cmd := exec.Command("bazel", "info", "bazel-bin")
	cmd.Dir = workspaceRoot
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type languageProtoRule struct {
	kind, name, protoRuleName, importPath string
}

type srcAndDest struct {
	src, dest string
}

func (r *languageProtoRule) getSrcAndDest(workspaceRoot, bazelBin, protoPath string) ([]srcAndDest, error) {
	protoRelpath := strings.TrimPrefix(protoPath, workspaceRoot)

	debugf("getSrcAndDest(%q, %q, %q)", workspaceRoot, bazelBin, protoPath)

	switch r.kind {
	case goProtoLibrary:
		wsRelpath := githubRepoRe.ReplaceAllLiteralString(r.importPath, "")
		if wsRelpath == r.importPath {
			return nil, fmt.Errorf("could not figure out workspace relative path for import %q", r.importPath)
		}
		srcDir := filepath.Join(bazelBin, filepath.Dir(protoRelpath), r.name+"_", r.importPath)
		debugf("globbing: %q", srcDir+"/*.pb.go")
		srcs, err := filepath.Glob(srcDir + "/*.pb.go")
		if err != nil {
			return nil, fmt.Errorf("could not find generated go files: %s", err)
		}

		res := []srcAndDest{}
		for _, src := range srcs {
			genBase := filepath.Base(src)
			dest := filepath.Join(workspaceRoot, wsRelpath, genBase)
			res = append(res, srcAndDest{src: src, dest: dest})
		}

		return res, nil
	case tsProtoLibrary:
		src := filepath.Join(bazelBin, filepath.Dir(protoRelpath), r.name+".d.ts")
		dest := filepath.Join(workspaceRoot, filepath.Dir(protoRelpath), r.name+".d.ts")
		return []srcAndDest{{src: src, dest: dest}}, nil

	}
	return nil, fmt.Errorf("unknown proto rule kind %q", r.kind)
}

type parsedBuildFile struct {
	protoFileToRule           map[string]string
	protoRuleToLangProtoRules map[string][]languageProtoRule
}

func (b *parsedBuildFile) getLangProtoRulesForProto(protoFile string) ([]languageProtoRule, bool) {
	basename := filepath.Base(protoFile)
	protoRule, ok := b.protoFileToRule[basename]
	if !ok {
		return nil, false
	}
	langRules, ok := b.protoRuleToLangProtoRules[protoRule]
	if !ok {
		return nil, false
	}
	return langRules, true
}

func parseBuildFile(buildFilePath string) (*parsedBuildFile, error) {
	buildFileContents, err := os.ReadFile(buildFilePath)
	if err != nil {
		return nil, err
	}
	buildFile, err := build.ParseBuild(filepath.Base(buildFilePath), buildFileContents)
	if err != nil {
		return nil, fmt.Errorf("could not parse BUILD file %q: %v", buildFilePath, err)
	}

	protoFileToRule := make(map[string]string)

	protoRules := buildFile.Rules("proto_library")
	for _, r := range protoRules {
		srcs := r.AttrStrings("srcs")
		if srcs == nil {
			return nil, fmt.Errorf("%s: proto rule %q does not have have srcs", buildFilePath, r.Name())
		}
		for _, src := range srcs {
			src = strings.TrimPrefix(src, ":")
			if protoFileToRule[src] != "" {
				return nil, fmt.Errorf("%s: src file %q appears in multiple proto rules", buildFilePath, src)
			}
			protoFileToRule[src] = r.Name()
		}
	}

	protoRuleToLangProtoRules := make(map[string][]languageProtoRule)

	goProtoRules := buildFile.Rules("")
	for _, r := range goProtoRules {
		if r.Kind() != goProtoLibrary && r.Kind() != tsProtoLibrary {
			continue
		}

		protoRule := r.AttrString("proto")
		if protoRule == "" {
			return nil, fmt.Errorf("%s: go proto rule %q missing proto attribute", buildFilePath, r.Name())
		}
		if !strings.HasPrefix(protoRule, ":") {
			// fmt.Printf("%s: go proto rule %q has unsupported proto reference: %s\n", buildFilePath, r.Name(), protoRule)
			continue
		}

		importPath := ""
		if r.Kind() == goProtoLibrary {
			importPath = r.AttrString("importpath")
			if importPath == "" {
				return nil, fmt.Errorf("%s: go proto rule %q missing importpath attribute", buildFilePath, r.Name())
			}
		}

		protoRuleName := protoRule[1:]
		langProtoRule := languageProtoRule{
			kind:          r.Kind(),
			name:          r.Name(),
			protoRuleName: protoRule[1:],
			importPath:    importPath,
		}
		protoRuleToLangProtoRules[protoRuleName] = append(protoRuleToLangProtoRules[protoRuleName], langProtoRule)
	}

	return &parsedBuildFile{
		protoFileToRule:           protoFileToRule,
		protoRuleToLangProtoRules: protoRuleToLangProtoRules,
	}, nil
}

type result struct {
	created  int64
	upToDate int64
}

func syncProto(workspaceRoot string, protoFile string, buildFile *parsedBuildFile, result *result) error {
	debugf("> SYNC %q", protoFile)

	rules, ok := buildFile.getLangProtoRulesForProto(protoFile)
	if !ok {
		fmt.Printf("could not figure out proto rule for %q\n", protoFile)
		return nil
	}
	debugf("rules(%q) => %+#v", protoFile, rules)

	bazelBin, err := getBazelBinDir(workspaceRoot)
	if err != nil {
		return fmt.Errorf("failed to determine bazel bin dir: %s", err)
	}

	for _, rule := range rules {
		debugf("Visiting rule %q", rule.name)
		srcAndDestPaths, err := rule.getSrcAndDest(workspaceRoot, bazelBin, protoFile)
		if err != nil {
			return fmt.Errorf("failed to get src and dest paths for %q: %s", protoFile, err)
		}

		for _, srcAndDest := range srcAndDestPaths {
			debugf("src %q", srcAndDest.src)
			src := srcAndDest.src
			dest := srcAndDest.dest

			// Read the generated source
			sb, err := os.ReadFile(src)
			if err != nil {
				if os.IsNotExist(err) {
					// Skip; the generated source is not available.
					continue
				}
				return err
			}
			sourceContent := string(sb)
			if sourceContent == "" {
				return fmt.Errorf("file is unexpectedly empty: %s", protoFile)
			}

			// Read the existing target file
			db, err := os.ReadFile(dest)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			destContent := string(db)

			if sourceContent == destContent {
				atomic.AddInt64(&result.upToDate, 1)
				debugf("dst %q is up to date", dest)
				continue
			}

			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dest, sb, 0644); err != nil {
				return err
			}
			atomic.AddInt64(&result.created, 1)
		}
	}
	return nil
}

func copyGeneratedProtos(workspaceRoot string) (*result, error) {
	foundWorkspaceFile := false
	for _, filename := range []string{"WORKSPACE", "WORKSPACE.bazel", "MODULE.bazel"} {
		if _, err := os.Stat(filepath.Join(workspaceRoot, filename)); err == nil {
			foundWorkspaceFile = true
			break
		}
	}
	if !foundWorkspaceFile {
		return nil, fmt.Errorf("%q does not appear to be a Bazel workspace", workspaceRoot)
	}

	// Get proto source paths (use the git index for speed)
	var protos []string
	lsFiles := exec.Command("sh", "-c", `
		git ls-files --exclude-standard '*.proto'
		git ls-files --others --exclude-standard '*.proto'
	`)
	lsFiles.Dir = workspaceRoot
	stderr := &bytes.Buffer{}
	lsFiles.Stderr = stderr
	buf := &bytes.Buffer{}
	lsFiles.Stdout = buf
	if err := lsFiles.Run(); err != nil {
		// If we're not in a git repo, do nothing.
		if _, err := os.Stat(filepath.Join(workspaceRoot, ".git")); os.IsNotExist(err) {
			return &result{}, nil
		}
		return nil, fmt.Errorf("failed to list proto sources: git ls-files failed: %s", stderr.String())
	}
	for _, path := range strings.Split(buf.String(), "\n") {
		protos = append(protos, filepath.Join(workspaceRoot, path))
	}

	result := &result{}

	eg := errgroup.Group{}
	if debug {
		// Concurrency makes debug logs harder to read - disable.
		eg.SetLimit(1)
	}
	parser := newBuildFileParser()

	for _, proto := range protos {
		eg.Go(func() error {
			// For now only support build files named "BUILD".
			buildFilePath := filepath.Join(filepath.Dir(proto), "BUILD")
			buildFile, err := parser.Parse(buildFilePath)
			if err != nil {
				// Ignore protos that aren't direct children of Bazel packages.
				if os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("failed to parse BUILD file at %q: %v", buildFilePath, err)
			}
			if err := syncProto(workspaceRoot, proto, buildFile, result); err != nil {
				return err
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return result, nil
}

type Result[T any] struct {
	Err error
	Val T
}

// buildFileParser is a deduplicating, concurrency-safe BUILD file parser.
type buildFileParser struct {
	group singleflight.Group

	mu    sync.RWMutex
	cache map[string]*Result[*parsedBuildFile]
}

func newBuildFileParser() *buildFileParser {
	return &buildFileParser{
		cache: map[string]*Result[*parsedBuildFile]{},
	}
}

func (p *buildFileParser) Parse(path string) (*parsedBuildFile, error) {
	val, err, _ := p.group.Do(path, func() (val interface{}, err error) {
		p.mu.RLock()
		cached := p.cache[path]
		p.mu.RUnlock()
		if cached != nil {
			return cached.Val, cached.Err
		}

		defer func() {
			result := &Result[*parsedBuildFile]{}
			if err != nil {
				result.Err = err
			} else {
				result.Val = val.(*parsedBuildFile)
			}

			p.mu.Lock()
			p.cache[path] = result
			p.mu.Unlock()
		}()

		return parseBuildFile(path)
	})

	if err != nil {
		return nil, err
	}
	return val.(*parsedBuildFile), nil
}

func printf(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, msg, args...)
}

func fatalf(msg string, args ...any) {
	printf("pbsync: "+msg+"\n", args...)
	os.Exit(1)
}

func main() {
	start := time.Now()

	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			fatalf("failed to determine working dir: %s", err)
		}
		dirs = append(dirs, cwd)
	}

	total := &result{}

	for _, dir := range dirs {
		result, err := copyGeneratedProtos(dir)
		if err != nil {
			fatalf("failed to sync protos for workspace %s: %s", dir, err)
		}
		total.created += result.created
		total.upToDate += result.upToDate
	}
	if total.created > 0 {
		printf("🔄 ")
	} else {
		printf("\x1b[90m")
	}

	printf("pbsync: updated: %d, up to date: %d, duration: %s\x1b[m\n", total.created, total.upToDate, time.Since(start))
}
