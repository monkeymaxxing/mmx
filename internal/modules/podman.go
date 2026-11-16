package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// What the Podman modules share: podman_image (this file), podman_quadlet,
// and wait_for's podman_container_is_healthy. Each runs the node's podman
// command, as root for the system's containers and as an ordinary account,
// with that account's runtime directory, for the account's own. MMX links no
// Podman library and reads only what podman prints as JSON.

// subordinateIDFiles are where the system's account tools record each
// ordinary account's ranges of sub-user and sub-group IDs; tests replace the
// paths.
var subordinateIDFiles = []struct{ path, ids string }{{"/etc/subuid", "sub-user IDs"}, {"/etc/subgid", "sub-group IDs"}}

// nameServiceSwitchFile says where the node looks accounts up; tests replace
// it.
var nameServiceSwitchFile = "/etc/nsswitch.conf"

// podmanTool is the node's podman command for one task: the program, and
// the systemd of the account whose containers and images the task means.
type podmanTool struct {
	// module names the task's module in failures.
	module  string
	program string
	// manager is the system's systemd for root's containers, and the
	// account's own for an ordinary account's.
	manager UnitManager
}

// podman finds the podman command and, for an account, the account. A
// missing program or account is something an earlier task may still
// provide. field is the task's field that names the account.
func (run Runtime) podman(ctx context.Context, module, field, account string) (*podmanTool, *protocol.Result) {
	program, err := lookPath(ctx, "podman")
	if err != nil {
		r := run.notYet(fmt.Errorf("%s: %w", module, missingProgramError("podman")))
		return nil, &r
	}
	m, err := SystemdOf(account)
	if err != nil {
		r := run.accountFailure(fmt.Errorf("%s.%s: %w", module, field, podmanAccountError(err)))
		return nil, &r
	}
	return &podmanTool{module: module, program: program, manager: m}, nil
}

// podmanAccountError puts root's case in Podman's words: SystemdOf speaks of
// units.
func podmanAccountError(err error) error {
	if errors.Is(err, errRootHasNoSystemdOfItsOwn) {
		return errors.New("root uses the system's Podman containers and image store; omit the user account field to use the system's store")
	}
	return err
}

// command runs podman as root, or as the account with its runtime directory.
func (p *podmanTool) command(ctx context.Context, args ...string) protocol.Result {
	return p.manager.runAsAccount(ctx, p.program, args...) //nolint:nilaway // Callers check stop; Quadlet removal reaches this call only after successful construction or with objectExists established.
}

// failure is a failed podman command in the words of podmanCommandFailure.
func (p *podmanTool) failure(args []string, r protocol.Result) error {
	return fmt.Errorf("%s: %w", p.module, podmanCommandFailure(p.program, args, r)) //nolint:nilaway // Callers check stop; Quadlet removal sets objectExists only after using a successfully constructed tool.
}

// podmanCommandFailure is a failed podman or skopeo command with the end of
// what it wrote to standard error, and never what it wrote to standard
// output (failureTextFromStandardError). The task's login file is left out
// of the command line.
func podmanCommandFailure(program string, args []string, r protocol.Result) error {
	shown := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--authfile" {
			i++
			continue
		}
		shown = append(shown, args[i])
	}
	return fmt.Errorf("%s %s: %s", filepath.Base(program), strings.Join(shown, " "), failureTextFromStandardError(r))
}

// version asks podman for its version, such as 5.4.2.
func (p *podmanTool) version(ctx context.Context) (string, error) {
	args := []string{"version", "--format", "json"}
	// The version is the program's, whoever asks; root asks, which needs no
	// runtime directory of an account.
	r := Command(ctx, nil, "/", p.program, args...)
	if r.Status != "succeeded" {
		return "", p.failure(args, r)
	}
	var answer struct {
		Client struct {
			Version string
		}
	}
	if err := json.Unmarshal([]byte(r.Stdout), &answer); err != nil || answer.Client.Version == "" {
		return "", fmt.Errorf("%s: podman version --format json printed no version: %s", p.module, strings.TrimSpace(r.Stdout))
	}
	return answer.Client.Version, nil
}

// where names the image store or the containers the tool works on.
func (p *podmanTool) where(what string) string {
	if p.manager.Account == "" {
		return "the system's " + what
	}
	return "the " + what + " of account " + p.manager.Account //nolint:nilaway // PodmanImage checks stop before passing the tool to podmanImage.
}

// checkContainerAccount checks what Podman needs before it can run anything
// as an ordinary account: a home directory, and the account's own ranges of
// sub-user and sub-group IDs. An earlier task can still provide each, so a
// dryrun reports the task not checked.
func (run Runtime) checkContainerAccount(module string, m UnitManager) *protocol.Result {
	if m.Account == "" {
		return nil
	}
	if info, err := os.Stat(m.Home); err != nil || !info.IsDir() {
		r := run.notYet(fmt.Errorf("%s: account %s has no home directory at %s; Podman stores the account's containers and images in the home directory, so create the home directory in an earlier user node task with create_home_directory: true", module, m.Account, m.Home))
		return &r
	}
	for _, file := range subordinateIDFiles {
		found, err := hasSubordinateIDs(file.path, m)
		if err != nil {
			r := protocol.Failed(fmt.Errorf("%s: %w", module, err))
			return &r
		}
		if !found {
			r := run.notYet(fmt.Errorf("%s: account %s has no range of %s in %s; Podman needs the range to run containers as an ordinary account. The system's account tools add the range when they create an ordinary account, and add none for a system account: create the account in an earlier user node task with system: false, or add the range to an existing account with usermod --add-subuids and --add-subgids", module, m.Account, file.ids, file.path))
			return &r
		}
	}
	return nil
}

// hasSubordinateIDs reports whether file gives the account a range: a line
// that starts with the account's name or its user ID. A node that looks
// sub-IDs up somewhere else (a subid line in nsswitch.conf that names
// another source than files) is not judged by the file; podman reports a
// missing range itself there.
func hasSubordinateIDs(file string, m UnitManager) (bool, error) {
	if switched, err := os.ReadFile(nameServiceSwitchFile); err == nil {
		for _, line := range strings.Split(string(switched), "\n") {
			if sources, found := strings.CutPrefix(strings.TrimSpace(line), "subid:"); found && strings.TrimSpace(sources) != "files" {
				return true, nil
			}
		}
	}
	text, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(text), "\n") {
		owner, _, _ := strings.Cut(line, ":")
		if owner == m.Account || owner == fmt.Sprint(m.UID) {
			return true, nil
		}
	}
	return false, nil
}

// imageID is an image ID in the form MMX reports and compares: the digest of
// the image's configuration with its algorithm, sha256:<64 hexadecimal
// digits>. podman image inspect prints the digits alone.
func imageID(id string) string {
	if id == "" || strings.Contains(id, ":") {
		return id
	}
	return "sha256:" + id
}

// podmanImageState is one image of a store.
type podmanImageState struct {
	// ID is the image ID in MMX's form, "" when the store has no image of
	// the name.
	ID string
	// RepoDigests are the digests under which registries served the image,
	// each as repository@digest. For an image that a registry serves for
	// several platforms, Podman records the digest of the list and the
	// digest of this platform's entry.
	RepoDigests []string
}

// came reports whether the image is the one that a registry serves under
// digest.
func (image podmanImageState) came(digest string) bool {
	for _, served := range image.RepoDigests {
		if _, d, found := strings.Cut(served, "@"); found && d == digest {
			return true
		}
	}
	return false
}

// localImage asks podman for the image that reference names in the store
// now; its ID is "" when the store does not have it.
func (p *podmanTool) localImage(ctx context.Context, reference string) (podmanImageState, error) {
	// exists answers with its exit code alone: 0 when the store has the
	// image and 1 when not.
	exists := []string{"image", "exists", reference}
	r := p.command(ctx, exists...)
	if r.Status != "succeeded" {
		if r.ExitCode == 1 && ctx.Err() == nil {
			return podmanImageState{}, nil
		}
		return podmanImageState{}, p.failure(exists, r)
	}
	inspect := []string{"image", "inspect", "--format", "json", reference}
	r = p.command(ctx, inspect...)
	if r.Status != "succeeded" {
		return podmanImageState{}, p.failure(inspect, r)
	}
	var images []struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	if err := json.Unmarshal([]byte(r.Stdout), &images); err != nil || len(images) != 1 || images[0].ID == "" {
		return podmanImageState{}, fmt.Errorf("%s: podman image inspect printed no image ID for %s", p.module, reference)
	}
	return podmanImageState{ID: imageID(images[0].ID), RepoDigests: images[0].RepoDigests}, nil
}

// podmanRegistryAnswer is what a registry says about a reference without a
// download.
type podmanRegistryAnswer struct {
	// Digest is the digest of what the registry serves under the reference:
	// one image's manifest, or the list of an image for several platforms.
	// PlatformDigest is the digest of the manifest of the image for this
	// node's platform: the same for one image, the list's entry for several.
	// Podman records both with the image that a pull brings.
	Digest, PlatformDigest string
	// ImageID is the digest of the image's configuration, which is the
	// image ID that Podman gives the image, with one exception.
	ImageID string
	// ZstdChunked is the exception: the image has layers in the
	// zstd:chunked format, and for an image whose layers it pulled that way
	// Podman can compute an image ID from more than the configuration.
	ZstdChunked bool
}

// registryImage asks the registry which image reference names now, without
// downloading the image: skopeo reads the manifest, whose own digest is what
// the registry serves under the reference and whose config.digest is the
// image ID. A reference that names images for several platforms names a
// list; the entry for this node's platform is read with a second request.
// skopeo reads the registries.conf, certificates, and logins that podman
// reads, so it asks the registry that a pull would ask. authFile is the
// task's login file, "" for none.
func (p *podmanTool) registryImage(ctx context.Context, skopeo string, reference imageReference, authFile string) (podmanRegistryAnswer, error) {
	repository := reference.Registry + "/" + reference.Repository
	// skopeo takes a tag or a digest, never both; the digest decides.
	text := repository + ":" + reference.Tag
	if reference.Digest != "" {
		text = repository + "@" + reference.Digest
	}
	// The digests of the answers, in the order of the questions.
	var digests []string
	read := func(text string) (registryManifest, error) {
		var manifest registryManifest
		args := []string{"inspect", "--raw"}
		if authFile != "" {
			args = append(args, "--authfile", authFile)
		}
		args = append(args, "docker://"+text)
		r := p.manager.runAsAccount(ctx, skopeo, args...)
		if r.Status != "succeeded" {
			return manifest, podmanCommandFailure(skopeo, args, r)
		}
		if r.StdoutTruncated {
			return manifest, fmt.Errorf("the registry's manifest for %s is too large to read", text)
		}
		if err := json.Unmarshal([]byte(r.Stdout), &manifest); err != nil {
			return manifest, fmt.Errorf("the registry's answer for %s is not a manifest: %w", text, err)
		}
		// A manifest's digest is the SHA-256 of its bytes as the registry
		// sent them, which skopeo prints unchanged.
		sum := sha256.Sum256([]byte(r.Stdout))
		digests = append(digests, "sha256:"+hex.EncodeToString(sum[:]))
		return manifest, nil
	}
	manifest, err := read(text)
	if err != nil {
		return podmanRegistryAnswer{}, err
	}
	if len(manifest.Manifests) > 0 {
		digest := ""
		for _, entry := range manifest.Manifests {
			if entry.Platform.OS == imageOS && entry.Platform.Architecture == runtime.GOARCH {
				digest = entry.Digest
				break
			}
		}
		if digest == "" {
			return podmanRegistryAnswer{}, fmt.Errorf("%s has no image for this node's platform, %s/%s", text, imageOS, runtime.GOARCH)
		}
		if manifest, err = read(repository + "@" + digest); err != nil {
			return podmanRegistryAnswer{}, err
		}
	}
	if manifest.Config.Digest == "" {
		return podmanRegistryAnswer{}, fmt.Errorf("the registry's manifest for %s names no image configuration, so it does not say the image ID", text)
	}
	answer := podmanRegistryAnswer{Digest: digests[0], PlatformDigest: digests[len(digests)-1], ImageID: manifest.Config.Digest} //nolint:nilaway // Successful read appends a digest; the initial failed-read branch returns before either index is evaluated.
	for _, layer := range manifest.Layers {
		for annotation := range layer.Annotations {
			answer.ZstdChunked = answer.ZstdChunked || strings.Contains(annotation, "zstd-chunked")
		}
	}
	return answer, nil
}

// imageOS is the operating system of the images a node runs: a node is a
// Linux server. The architecture is the node runner's own.
const imageOS = "linux"

// registryManifest is what MMX reads of a registry's manifest: the image
// configuration's digest and the layers' annotations of one image, or the
// entries of a list.
type registryManifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

// PodmanImage is one Podman image on the node, pulled from a registry into
// the system's image store or an ordinary account's. The two login fields
// are private values that the module hands only to podman and skopeo as a
// login (resolveRegistryLogin).
type PodmanImage struct {
	Image                     string `yaml:"image" json:"image"`
	PullImage                 string `yaml:"pull_image" json:"pull_image"`
	RegistryUsername          string `yaml:"registry_username,omitempty" json:"registry_username,omitempty"`
	RegistryPassword          string `yaml:"registry_password,omitempty" json:"registry_password,omitempty"`
	InImageStoreOfUserAccount string `yaml:"in_image_store_of_user_account,omitempty" json:"in_image_store_of_user_account,omitempty"`
}

// podmanImageDescription is what mmx describe-module shows for podman_image.
var podmanImageDescription = Description{
	Name: "podman_image", Topic: TopicContainers, Schema: reflect.TypeOf(PodmanImage{}),
	Summary: "one Podman image on the node, pulled from a registry into the system's image store or an ordinary account's; value has image_id and previous_image_id",
	Fields: []Field{
		{Name: "image", Required: true, Notes: "a full image reference: registry, image name, and a tag or a digest, such as registry.example.com/shop/web:2.4; a reference without a registry, such as nginx:1.27, is rejected, because Podman would have to guess the registry"},
		{Name: "pull_image", Required: true, Values: pullImageChoices, Notes: "no default: when_missing pulls only when the store does not have the image, so a rerun never follows a tag that moved; always asks the registry on every run and pulls when the reference points to another image"},
		{Name: "registry_username", Notes: "the login for the registry named in image; both registry_username and registry_password, or neither"},
		{Name: "registry_password", Notes: "a private value, normally a secret; it reaches Podman through a file under /run that only the account that runs Podman can read and that MMX removes after the task, never a command line or a home directory; with in_image_store_of_user_account that account runs Podman, so it can read the password while the task pulls; a secret in the login does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the secret"},
		{Name: "in_image_store_of_user_account", Notes: "an account other than root: the image goes into that account's own image store, which the account's containers use, and MMX turns on lingering for the account so that its services start at boot and keep running after logout; without it, the system's (root's) store"},
	},
	Example: "- podman_image:\n    image: registry.example.com/shop/web:2.4\n    pull_image: when_missing\n",
	Results: []string{"status", "change", "reason", "value.image_id", "value.previous_image_id"},
	Dryrun:  "pulls nothing; when_missing reports would_change for an image the store does not have; always asks the registry with skopeo, without downloading the image: unchanged when the store's image is the one the registry serves for the node's platform now, would_change with both image IDs when not, and not_checked when the registry cannot be reached, skopeo is not installed, or only a pull can say whether the image differs (an image with zstd:chunked layers that the registry serves under another digest than the store's image came under); a not_checked result names the store's image as value.previous_image_id and predicts no restart of a service that follows an image the store has",
}

// podmanImageRule checks a resolved reference: a registry, and a tag or a
// digest.
var podmanImageRule = parsed(func(text string) error {
	_, err := checkImageReference(text, true)
	return err
})

// Prepare checks the inputs when the project loads.
func (p *PodmanImage) Prepare(base string) error {
	switch {
	case p.Image == "":
		return invalid("image", "podman_image.image is required: a full image reference, such as registry.example.com/shop/web:2.4")
	case p.PullImage == "":
		return invalid("pull_image", "podman_image needs pull_image: when_missing or always; there is no default, because it decides whether a rerun deploys a newer image")
	case !slices.Contains(pullImageChoices, p.PullImage):
		return invalid("pull_image", "pull_image must be "+strings.Join(pullImageChoices, " or "))
	case (p.RegistryUsername == "") != (p.RegistryPassword == ""):
		return invalid("registry_username", "registry_username and registry_password come together")
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *PodmanImage) Arguments(arguments *ArgumentList) {
	arguments.Public("image", &p.Image, podmanImageRule)
	if p.InImageStoreOfUserAccount != "" {
		arguments.Public("in_image_store_of_user_account", &p.InImageStoreOfUserAccount, ordinaryContainerAccount)
	}
	if p.RegistryUsername != "" {
		arguments.LoginHandedOnlyToATool("registry_username", p.RegistryUsername, scalar)
	}
	if p.RegistryPassword != "" {
		arguments.LoginHandedOnlyToATool("registry_password", p.RegistryPassword, scalar)
	}
}

// ordinaryContainerAccount is the rule for an account that has its own
// containers and images: any account but root, whose are the system's.
var ordinaryContainerAccount = func(v any) error {
	if err := accountNameRule(v); err != nil {
		return err
	}
	if s, _ := value.Scalar(v); s == "root" {
		return errors.New("root uses the system's Podman containers and image store; omit the user account field to use the system's store")
	}
	return nil
}

// Run does the module's work on the node.
func (p *PodmanImage) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.PodmanImage(ctx, *p, task.Scope)
}

// podmanImageNotChecked is the result of a dryrun that could not see whether
// a pull would bring another image. It names the image that the store has,
// so that nothing is predicted for what follows that image (StateAfter).
func podmanImageNotChecked(reason, previous string) protocol.Result {
	r := podmanImageResult("unknown", "", previous)
	r.Status, r.Reason = "not_checked", reason
	return r
}

// podmanImageResult is a task's result with its two image IDs; an ID the
// task does not know is null.
func podmanImageResult(change, id, previous string) protocol.Result {
	r := protocol.Success(change)
	v := map[string]any{"image_id": nil, "previous_image_id": nil}
	if id != "" {
		v["image_id"] = id
	}
	if previous != "" {
		v["previous_image_id"] = previous
	}
	r.Value = v
	return r
}

// PodmanImage pulls the image when the store does not have it, or, with
// pull_image: always, when the registry points the reference at another
// image. It compares what podman image inspect says before and after, never
// what the pull prints.
func (run Runtime) PodmanImage(ctx context.Context, p PodmanImage, s value.Scope) protocol.Result {
	tool, stop := run.podman(ctx, "podman_image", "in_image_store_of_user_account", p.InImageStoreOfUserAccount)
	if stop != nil {
		return *stop
	}
	//nolint:nilaway // The preceding stop check excludes every nil return from podman.
	if stop := run.checkContainerAccount("podman_image", tool.manager); stop != nil {
		return *stop
	}
	// Podman run as an account without the account's runtime directory keeps
	// its state under /tmp from then on and leaves its pause process in the
	// node job's own process group. The runtime directory is there while the
	// account's systemd runs, which lingering starts and keeps. So lingering
	// goes on before the task can know whether its pull succeeds; a task that
	// fails after that says so in its reason.
	reportLingering, stop := run.keepAccountServicesRunning(ctx, tool.manager) //nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	if stop != nil {
		return *stop
	}
	var result protocol.Result
	switch {
	//nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	case tool.manager.Running():
		result = run.podmanImage(ctx, tool, p, s)
	//nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	case run.Dryrun && !tool.manager.Lingering():
		// The task would turn lingering on; until then the account has no
		// runtime directory, and its image store cannot be read.
		result = podmanImageResult("would_change", "", "")
		reportLingering(&result)
		result.Reason += "; the account's image store can be read only after that"
		return result
	default:
		result = run.notYet(fmt.Errorf("podman_image: the systemd of account %s does not run, so the account has no runtime directory %s for Podman; start it with systemctl start user@%d.service", tool.manager.Account, tool.manager.runtimeDirectory(), tool.manager.UID)) //nolint:nilaway // The preceding constructor failure check guarantees that tool exists.
	}
	reportLingering(&result)
	return result
}

// podmanImage does the task's work once podman can run for the store.
func (run Runtime) podmanImage(ctx context.Context, tool *podmanTool, p PodmanImage, s value.Scope) protocol.Result {
	// The reference arrives checked; only its parts are read here.
	reference, _ := parseImageReference(p.Image)
	before, err := tool.localImage(ctx, p.Image)
	if err != nil {
		return protocol.Failed(err)
	}
	previous := before.ID
	// A digest names one image forever, so the registry can only be asked
	// for an image the store does not have, as with when_missing.
	if p.PullImage == "when_missing" || reference.Digest != "" {
		if previous != "" {
			return podmanImageResult("unchanged", previous, previous)
		}
		if run.Dryrun && p.PullImage == "when_missing" {
			r := podmanImageResult("would_change", "", "")
			r.Reason = tool.where("image store") + " does not have the image " + p.Image
			return r
		}
	}
	// The task's login for the registry goes into a file in the task's
	// private directory (makePrivateTaskDirectory), which only the account
	// running podman can read. The password is then on no command line and in
	// no home directory. For an account's image store that account runs
	// podman, so the file is the account's for as long as the task runs.
	authFile := ""
	if p.RegistryUsername != "" {
		login, err := resolveRegistryLogin(s, reference.Registry, "podman_image", p.RegistryUsername, p.RegistryPassword)
		if err != nil {
			return protocol.Failed(err)
		}
		//nolint:nilaway // PodmanImage passes the tool here only after checking constructor failure.
		directory, remove, err := run.makePrivateTaskDirectory("podman_image", tool.manager.credential, func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "auth.json"), registryLoginFile([]registryLogin{login}), 0600)
		})
		if err != nil {
			return protocol.Failed(fmt.Errorf("podman_image: the private file for the registry login: %w", err))
		}
		defer remove()
		authFile = filepath.Join(directory, "auth.json")
	}
	if run.Dryrun {
		skopeo, err := lookPath(ctx, "skopeo")
		if err != nil {
			return podmanImageNotChecked("podman_image: a dryrun of pull_image: always asks the registry with the program skopeo, which is not installed ("+missingProgramError("skopeo").Error()+"). A run needs only Podman", previous)
		}
		served, err := tool.registryImage(ctx, skopeo, reference, authFile)
		if err != nil {
			return podmanImageNotChecked(fmt.Sprintf("podman_image: the registry does not say which image %s names: %v", p.Image, err), previous)
		}
		// A pull brings another image unless the store's image is the one
		// that the registry serves for this node's platform now. It is that
		// image when Podman recorded it under the digest of the registry's
		// answer or of the manifest for this platform, whatever image ID
		// Podman gave it. It is also that image when it has the ID that the
		// manifest names: a registry can change its list of platforms and
		// keep this platform's image, and an image that never came from a
		// registry, such as one built on the node, has no recorded digest.
		if previous != "" && (before.came(served.Digest) || before.came(served.PlatformDigest) || previous == served.ImageID) {
			return podmanImageResult("unchanged", previous, previous)
		}
		if served.ZstdChunked {
			// The image ID of the image to come is not known. So nothing
			// says whether the store's image, which came under another
			// digest, is the same image; a missing image is another one.
			now := fmt.Sprintf("the registry now serves %s under the digest %s", p.Image, served.PlatformDigest)
			if previous != "" {
				return podmanImageNotChecked("podman_image: "+now+", and the image in "+tool.where("image store")+
					" came under another digest. The image's layers are in the zstd:chunked format, for which Podman can compute another image ID than the registry's manifest names, so only a pull can say whether it brings another image", previous)
			}
			r := podmanImageResult("would_change", "", "")
			r.Reason = now + ", and " + tool.where("image store") + " does not have the image"
			return r
		}
		id := served.ImageID
		r := podmanImageResult("would_change", id, previous)
		r.Reason = fmt.Sprintf("the registry names the image %s for %s now", id, p.Image)
		if previous == "" {
			r.Reason += ", and " + tool.where("image store") + " does not have the image"
		}
		return r
	}
	if err := run.Mutation("podman image " + p.Image); err != nil {
		return protocol.Failed(err)
	}
	pull := []string{"pull", "--quiet"}
	if authFile != "" {
		pull = append(pull, "--authfile", authFile)
	}
	pull = append(pull, p.Image)
	pulled := tool.command(ctx, pull...)
	// The store says what the node has now, whatever the pull printed and
	// also after a pull that failed part of the way.
	after, err := tool.localImage(ctx, p.Image)
	if err != nil {
		r := protocol.Failed(errors.Join(pullFailure(tool, pull, pulled), err))
		r.Change = "unknown"
		return r
	}
	id := after.ID
	change := "unchanged"
	if id != previous {
		change = "changed"
	}
	if failure := pullFailure(tool, pull, pulled); failure != nil {
		r := protocol.Failed(failure)
		r.Change = change
		return r
	}
	if id == "" {
		r := protocol.Failed(fmt.Errorf("podman_image: podman pull %s succeeded, but %s does not have the image", p.Image, tool.where("image store")))
		r.Change = "unknown"
		return r
	}
	return podmanImageResult(change, id, previous)
}

// pullFailure is a failed pull as an error, nil for one that succeeded.
func pullFailure(tool *podmanTool, args []string, r protocol.Result) error {
	if r.Status == "succeeded" {
		return nil
	}
	return tool.failure(args, r)
}

// TrackedInputs lists the image, which counts by its image ID.
func (p *PodmanImage) TrackedInputs() []TrackedInput {
	name := "podman_image " + p.Image
	if p.InImageStoreOfUserAccount != "" {
		name += " of account " + p.InImageStoreOfUserAccount
	}
	return []TrackedInput{{Kind: "installed", Path: name, State: p}}
}

// ReadState is the ID of the image in the store now, "" when the store does
// not have it or cannot be asked yet: without podman, without the account,
// or while the account's systemd does not run.
func (p *PodmanImage) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	tool, stop := Runtime{}.podman(ctx, "podman_image", "in_image_store_of_user_account", p.InImageStoreOfUserAccount)
	//nolint:nilaway // A nil tool always carries a non-nil stop; || short-circuits before accessing its manager.
	if stop != nil || !tool.manager.Running() {
		return "", nil
	}
	image, err := tool.localImage(ctx, p.Image)
	return image.ID, err
}

// StateAfter is what a dryrun predicts for the image (imageStateAfter). A
// dryrun cannot see whether a pull would bring another image than the
// store's when the registry does not answer, skopeo is missing, or only a
// pull can tell.
func (p *PodmanImage) StateAfter(r protocol.Result) (string, bool) { return imageStateAfter(r) }

// UnknownInADryrun lists the result fields a dryrun cannot know: the ID of
// an image it would pull, where Podman tells it only after the pull.
func (p *PodmanImage) UnknownInADryrun(r protocol.Result) map[string]string {
	return imageUnknownInADryrun(r)
}
