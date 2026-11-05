// Package historycompat implements a read-only, deliberately restricted preflight.
// It does not guarantee exclusion of arbitrary external native processes. Callers
// must hold their operation leases and preserve the stopped-writer prerequisite.
package historycompat

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "os"
 "path/filepath"
 "runtime"
 "strings"

 "github.com/pelletier/go-toml/v2"
)

const NativeVersion = "0.166.0"

// ArtifactPin must originate from the trusted packaged release manifest, never
// environment variables, user configuration, or the artifact's own output.
// Digest pins the native executable itself, not an npm/shell launcher.
type ArtifactPin struct { SHA256, Version string }
type Artifact struct { Path string; Pin ArtifactPin }

// ProcessScanner must fail on incomplete inspection as well as known writers.
// A successful observation is not a lock and cannot prevent later process starts.
type ProcessScanner interface { CheckStopped(context.Context) error }

// ConfigurationEvidence covers sources a filesystem scan cannot establish:
// packaged defaults, cloud managed bundles, application/thread config injection,
// and loader overrides. The adapter must verify the exact pinned native release,
// absence of overrides/dynamic storage policy, and return a stable fingerprint.
// A user-supplied boolean or environment assertion is not adequate evidence.
type ConfigurationEvidence interface { Fingerprint(context.Context, string, string) (string, error) }

type Request struct {
 StoppedWritersAcknowledged bool
 ProfileHome, ProjectRoot string
 Native, Helper Artifact
 // Environment and Args must be the complete environment/arguments that will
 // actually be passed to native Codex, not a filtered subset.
 Environment, Args []string
 Scanner ProcessScanner
 Configuration ConfigurationEvidence
}

type observed struct { path, digest string; info os.FileInfo; absent bool }
// Snapshot's contents are private so callers cannot manufacture a successful
// preflight. It records identity as well as bytes, including absent config paths.
type Snapshot struct { files []observed; evidence string; home, project string; homeInfo, projectInfo os.FileInfo; launch string }

// Check never executes an artifact, opens SQLite, creates directories or writes
// profile data. Identity comes from packaged hashes and exact manifest version.
func Check(ctx context.Context, r Request) (*Snapshot, error) { return check(ctx, r, runtime.GOOS, "/etc/codex", "/") }

func check(ctx context.Context, r Request, goos, system, boundary string) (*Snapshot, error) {
 if goos != "linux" { return nil, errors.New("native history projection is only validated on Linux") }
 if !r.StoppedWritersAcknowledged { return nil, errors.New("acknowledge that all Codex writers are stopped and will remain stopped during synchronization") }
 if r.Scanner == nil || r.Configuration == nil { return nil, errors.New("complete process and effective configuration inspection are required") }
 for _, e := range r.Environment {
  k, _, _ := strings.Cut(e, "=")
  if strings.EqualFold(k, "CODEX_SQLITE_HOME") { return nil, errors.New("CODEX_SQLITE_HOME is unsupported, including an empty assignment") }
 }
 // Initial prototype accepts only an empty interactive invocation. Subcommands
 // can change the server/configuration route just as flags can.
 if len(r.Args)!=0 { return nil, errors.New("native passthrough arguments are not yet validated for history synchronization") }
 home, err := directory(r.ProfileHome); if err != nil { return nil, err }
 project, err := directory(r.ProjectRoot); if err != nil { return nil, err }
 seenEnv:=map[string]bool{}
 for _,e:=range r.Environment { k,v,ok:=strings.Cut(e,"=");if !ok||k==""||seenEnv[k]{return nil,errors.New("invalid or duplicate launch environment entry")};seenEnv[k]=true;if k=="CODEX_HOME"&&v!=home{return nil,errors.New("launch CODEX_HOME differs from inspected profile")}}
 hi, err := os.Lstat(home); if err != nil { return nil, err }; pi, err := os.Lstat(project); if err != nil { return nil, err }
 launch, err := json.Marshal(struct{ Environment, Args []string }{r.Environment,r.Args}); if err != nil { return nil, err }; launchHash:=sha256.Sum256(launch)
 s := &Snapshot{home:home, project:project, homeInfo:hi, projectInfo:pi, launch:hex.EncodeToString(launchHash[:])}
 for _, a := range []Artifact{r.Native,r.Helper} {
  if a.Pin.Version != NativeVersion { return nil, errors.New("artifact pin must target exact native version 0.166.0") }
  digest, err := hex.DecodeString(a.Pin.SHA256); if err != nil || len(digest)!=sha256.Size { return nil, errors.New("missing or invalid packaged artifact digest") }
  o, err := observe(a.Path, false); if err != nil { return nil, err }
  if o.info.Mode()&0111 == 0 || o.digest != strings.ToLower(a.Pin.SHA256) { return nil, errors.New("artifact executable identity does not match packaged pin") }
  s.files=append(s.files,o)
 }
 // See openai/codex rust-v0.166.0 config/src/loader/{mod,layer_io}.rs.
 // Reject present system/managed/project layers rather than approximate trust
 // or precedence. Scan all ancestors, a conservative superset of discovery.
 forbidden:=[]string{filepath.Join(system,"config.toml"),filepath.Join(system,"requirements.toml"),filepath.Join(system,"managed_config.toml"),filepath.Join(project,"config.toml")}
 for p:=project;;p=filepath.Dir(p) { forbidden=append(forbidden,filepath.Join(p,".codex","config.toml")); if p==boundary||filepath.Dir(p)==p {break} }
 for _, p:=range forbidden {
  o,err:=observe(p,true); if err!=nil{return nil,err}; s.files=append(s.files,o)
  if !o.absent { return nil,fmt.Errorf("unsupported configuration layer: %s",p) }
 }
 p:=filepath.Join(home,"config.toml"); o,err:=observe(p,true); if err!=nil{return nil,err};s.files=append(s.files,o)
 if !o.absent {
  data,err:=os.ReadFile(p);if err!=nil{return nil,err}
  var cfg map[string]any
  if err:=toml.Unmarshal(data,&cfg);err!=nil{return nil,errors.New("invalid profile TOML")}
  // These can select additional layers or change storage/discovery semantics.
  for _,k:=range []string{"sqlite_home","profile","profiles","project_root_markers","config_server","managed_config","requirements"} { if _,ok:=cfg[k];ok{return nil,fmt.Errorf("unsupported profile configuration key: %s",k)} }
  sum:=sha256.Sum256(data);if hex.EncodeToString(sum[:])!=o.digest{return nil,errors.New("configuration changed during inspection")}
 }
 s.evidence,err=r.Configuration.Fingerprint(ctx,home,project);if err!=nil{return nil,err};if s.evidence==""{return nil,errors.New("incomplete effective configuration evidence")}
 if err:=r.Scanner.CheckStopped(ctx);err!=nil{return nil,err}
 if err:=s.verifyFiles();err!=nil{return nil,err}
 return s,nil
}

// Revalidate requires a fresh request containing the actual current launch
// inputs. It detects changed observations, not changes occurring after return.
func (s *Snapshot) Revalidate(ctx context.Context,r Request) error {
 if s==nil{return errors.New("missing preflight snapshot")}
 next,err:=Check(ctx,r);if err!=nil{return err}
 return s.equal(next)
}
func(s *Snapshot) equal(n *Snapshot)error{
 if s.home!=n.home||s.project!=n.project||s.evidence!=n.evidence||s.launch!=n.launch||!os.SameFile(s.homeInfo,n.homeInfo)||!os.SameFile(s.projectInfo,n.projectInfo)||len(s.files)!=len(n.files){return errors.New("preflight inputs changed")}
 for i,a:=range s.files {b:=n.files[i];if a.path!=b.path||a.absent!=b.absent||a.digest!=b.digest||(!a.absent&&!os.SameFile(a.info,b.info)){return errors.New("preflight file identity changed")}}
 return nil
}
func(s *Snapshot)verifyFiles()error{for _,d:=range []struct{path string; info os.FileInfo}{{s.home,s.homeInfo},{s.project,s.projectInfo}}{ if err:=safeComponents(d.path);err!=nil{return err};i,err:=os.Lstat(d.path);if err!=nil{return err};if !os.SameFile(d.info,i){return errors.New("directory replaced during preflight")}};for _,a:=range s.files{b,err:=observe(a.path,a.absent);if err!=nil{return err};if a.absent!=b.absent||a.digest!=b.digest||(!a.absent&&!os.SameFile(a.info,b.info)){return errors.New("files changed during preflight")}};return nil}
func directory(p string)(string,error){if !filepath.IsAbs(p){return "",errors.New("absolute profile/project paths required")};p=filepath.Clean(p);if err:=safeComponents(p);err!=nil{return "",err};i,err:=os.Lstat(p);if err!=nil{return "",err};if !i.IsDir(){return "",errors.New("profile/project must already exist as directories")};return p,nil}
func safeComponents(p string)error{
 if !filepath.IsAbs(p){return errors.New("absolute paths required")}
 for q:=filepath.Clean(p);;q=filepath.Dir(q){i,err:=os.Lstat(q);if err!=nil&&!os.IsNotExist(err){return err};if err==nil&&i.Mode()&os.ModeSymlink!=0{return fmt.Errorf("symlink component refused: %s",q)};if filepath.Dir(q)==q{break}}
 return nil
}
func observe(p string,allowMissing bool)(observed,error){
 o:=observed{path:filepath.Clean(p)};if err:=safeComponents(p);err!=nil{return o,err}
 i,err:=os.Lstat(p);if os.IsNotExist(err)&&allowMissing{o.absent=true;return o,nil};if err!=nil{return o,err}
 if !i.Mode().IsRegular(){return o,fmt.Errorf("nonregular file refused: %s",p)}
 if !singleLink(i){return o,fmt.Errorf("hardlinked file or unsupported identity refused: %s",p)}
 f,err:=os.Open(p);if err!=nil{return o,err};defer f.Close();fi,err:=f.Stat();if err!=nil{return o,err};if !os.SameFile(i,fi){return o,errors.New("file replaced during open")}
 h:=sha256.New();if _,err=io.Copy(h,f);err!=nil{return o,err};after,err:=f.Stat();if err!=nil{return o,err};if fi.Size()!=after.Size()||!fi.ModTime().Equal(after.ModTime()){return o,errors.New("file changed during read")}
 o.info=fi;o.digest=hex.EncodeToString(h.Sum(nil));return o,nil
}
