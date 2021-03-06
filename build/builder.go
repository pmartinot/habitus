package build

import (
	"archive/tar"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/cloud66/habitus/configuration"
	"github.com/cloud66/habitus/squash"
	"github.com/dchest/uniuri"
	"github.com/docker/docker/builder/dockerfile/parser"
	"github.com/fsouza/go-dockerclient"
	"github.com/satori/go.uuid"
)

// Builder is a simple Dockerfile builder
type Builder struct {
	Build    *Manifest
	UniqueID string // unique id for this build sequence. This is used for multi-tenanted environments
	Conf     *configuration.Config

	config    *tls.Config
	docker    docker.Client
	auth      *docker.AuthConfigurations
	builderId string // unique id for this builder session (used internally)
	wg        sync.WaitGroup
}

// NewBuilder creates a new builder in a new session
func NewBuilder(manifest *Manifest, conf *configuration.Config) *Builder {
	b := Builder{}
	b.Build = manifest
	b.UniqueID = conf.UniqueID
	b.Conf = conf
	b.builderId = uuid.NewV4().String()

	endpoint, err := url.Parse(b.Conf.DockerHost)
	if err != nil {
		b.Conf.Logger.Fatalf("Invalid host: %s", err.Error())
		return nil
	}

	var client *docker.Client
	if endpoint.Scheme == "unix" {
		client, err = docker.NewClient(endpoint.String())
	} else {
		if conf.UseTLS {
			certPath := b.Conf.DockerCert
			ca := path.Join(certPath, "ca.pem")
			cert := path.Join(certPath, "cert.pem")
			key := path.Join(certPath, "key.pem")
			client, err = docker.NewTLSClient(endpoint.String(), cert, key, ca)
		} else {
			client, err = docker.NewClient(endpoint.String())
		}
	}

	if err != nil {
		b.Conf.Logger.Fatal(err.Error())
		return nil
	}

	b.docker = *client

	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		b.Conf.Logger.Fatalf("Failed to find the current home")
	}

	if _, err := os.Stat(filepath.Join(homeDir, ".dockercfg")); err == nil {
		authStream, err := os.Open(filepath.Join(homeDir, ".dockercfg"))
		if err != nil {
			b.Conf.Logger.Fatal("Unable to read .dockercfg file")
		}
		defer authStream.Close()

		auth, err := docker.NewAuthConfigurations(authStream)
		if err != nil {
			b.Conf.Logger.Fatalf("Invalid .dockercfg: %s", err.Error())
		}
		b.auth = auth
	}

	if err != nil {
		b.Conf.Logger.Fatalf("Failed to connect to Docker daemon %s", err.Error())
	}

	return &b
}

// StartBuild runs the build process end to end
func (b *Builder) StartBuild() error {

	var hostArtifactRoots []string
	if !b.Conf.KeepArtifacts {
		b.Conf.Logger.Debug("Collecting artifact information")
		hostArtifactRoots = b.collectHostArtifactRoots()
	}

	b.Conf.Logger.Debugf("Building %d steps", len(b.Build.Steps))
	for i, s := range b.Build.Steps {
		b.Conf.Logger.Debugf("Step %d - %s: %s", i, s.Label, s.Name)
	}

	for _, levels := range b.Build.buildLevels {
		for _, s := range levels {
			b.wg.Add(1)
			go func(st Step) {
				b.Conf.Logger.Debugf("Parallel build for %s", st.Name)
				defer b.wg.Done()

				err := b.BuildStep(&st)
				if err != nil {
					b.Conf.Logger.Fatalf("Build for step %s failed due to %s", st.Name, err.Error())
				}
			}(s)
		}

		b.wg.Wait()
	}

	if !b.Conf.KeepArtifacts {
		// remove all artifacts created on the host
		for _, hostArtifactRoot := range hostArtifactRoots {
			b.Conf.Logger.Debugf("Removing artifact path: %s\n", hostArtifactRoot)
			// this path might be removed already due to overlapping
			// values; so we don't care if this fails
			os.RemoveAll(hostArtifactRoot)
		}
	}

	if b.Conf.KeepSteps {
		return nil
	}

	if len(b.Build.Steps) < 1 {
		b.Conf.Logger.Fatal("No build steps found")
	}

	// Clear after yourself: images, containers, etc (optional for premium users)
	// except last step
	for _, s := range b.Build.Steps[:len(b.Build.Steps)-1] {
		b.Conf.Logger.Debugf("Removing unwanted image %s", b.uniqueStepName(&s))
		rmiOptions := docker.RemoveImageOptions{Force: b.Conf.FroceRmImages, NoPrune: b.Conf.NoPruneRmImages}
		err := b.docker.RemoveImageExtended(b.uniqueStepName(&s), rmiOptions)
		if err != nil {
			return err
		}
	}

	return nil
}

// collects all existing artifact roots that are created
// during the build process and saved on the host so they
// can be removed at the end of the build process
func (b *Builder) collectHostArtifactRoots() []string {
	var hostArtifactRoots []string
	if !b.Conf.KeepArtifacts {
		for _, step := range b.Build.Steps {
			for _, artifact := range step.Artifacts {
				// get the projected relative path to the host file
				absHostFile := path.Join(b.Conf.Workdir, artifact.Dest, filepath.Base(artifact.Source))
				// use a regex to hand path expansion (ie. ../../)
				relHostFile := regexp.MustCompile(fmt.Sprintf("^%s/+", b.Conf.Workdir)).ReplaceAllString(absHostFile, "")
				// remove trailing /
				relHostFile = regexp.MustCompile("/$").ReplaceAllString(relHostFile, "")
				parts := strings.Split(relHostFile, "/")
				currentPath := b.Conf.Workdir
				for _, part := range parts {
					currentPath = path.Join(currentPath, part)
					if _, err := os.Stat(currentPath); os.IsNotExist(err) {
						// everything from this point down should be deleted
						hostArtifactRoots = append(hostArtifactRoots, currentPath)
						break
					}
				}
			}
		}
	}
	return hostArtifactRoots
}

// provides a name for the image
// it always adds the UID (if provided) to the end of the name
// keeping the tag intact if it exists
func (b *Builder) uniqueStepName(step *Step) string {
	if b.UniqueID == "" {
		return step.Name
	}

	newName := step.Name
	if strings.Contains(step.Name, ":") {
		parts := strings.Split(step.Name, ":")
		newName = parts[0] + "-" + b.UniqueID + ":" + parts[1]
	} else {
		newName = step.Name + "-" + b.UniqueID
	}

	return strings.ToLower(newName)
}

// BuildStep builds a single step
func (b *Builder) BuildStep(step *Step) error {
	b.Conf.Logger.Noticef("Building %s", step.Name)
	// fix the Dockerfile
	err := b.replaceFromField(step)
	if err != nil {
		return err
	}

	buildArgs := []docker.BuildArg{}
	for _, s := range b.Conf.BuildArgs {
		buildArgs = append(buildArgs, docker.BuildArg{Name: s.Key, Value: s.Value})
	}
	// call Docker to build the Dockerfile (from the parsed file)

	b.Conf.Logger.Infof("Building the %s image from %s", b.uniqueStepName(step), filepath.Base(b.uniqueDockerfile(step)))
	opts := docker.BuildImageOptions{
		Name:                b.uniqueStepName(step),
		Dockerfile:          filepath.Base(b.uniqueDockerfile(step)),
		NoCache:             b.Conf.NoCache,
		SuppressOutput:      b.Conf.SuppressOutput,
		RmTmpContainer:      b.Conf.RmTmpContainers,
		ForceRmTmpContainer: b.Conf.ForceRmTmpContainer,
		OutputStream:        os.Stdout, // TODO: use a multi writer to get a stream out for the API
		ContextDir:          b.Conf.Workdir,
		BuildArgs:           buildArgs,
	}

	if b.auth != nil {
		opts.AuthConfigs = *b.auth
	}

	err = b.docker.BuildImage(opts)

	if err != nil {
		return err
	}

	// if there are any artifacts to be picked up, create a container and copy them over
	// we also need a container if there are cleanup commands
	if len(step.Artifacts) > 0 || len(step.Cleanup.Commands) > 0 || step.Command != "" {
		b.Conf.Logger.Notice("Building container based on the image")

		// create a container
		container, err := b.createContainer(step)
		if err != nil {
			return err
		}

		if !b.Conf.NoSquash && len(step.Cleanup.Commands) > 0 {
			// start the container
			b.Conf.Logger.Noticef("Starting container %s to run cleanup commands", container.ID)
			startOpts := &docker.HostConfig{}
			err := b.docker.StartContainer(container.ID, startOpts)
			if err != nil {
				return err
			}

			for _, cmd := range step.Cleanup.Commands {
				b.Conf.Logger.Debugf("Running cleanup command %s on %s", cmd, container.ID)
				// create an exec for the commands
				execOpts := docker.CreateExecOptions{
					Container:    container.ID,
					AttachStdin:  false,
					AttachStdout: true,
					AttachStderr: true,
					Tty:          false,
					Cmd:          strings.Split(cmd, " "),
				}
				execObj, err := b.docker.CreateExec(execOpts)
				if err != nil {
					return err
				}

				success := make(chan struct{})

				go func() {
					startExecOpts := docker.StartExecOptions{
						OutputStream: os.Stdout,
						ErrorStream:  os.Stderr,
						RawTerminal:  true,
					}

					if err := b.docker.StartExec(execObj.ID, startExecOpts); err != nil {
						b.Conf.Logger.Errorf("Failed to run cleanup commands %s", err.Error())
					}
					success <- struct{}{}
				}()
				<-success
			}

			// commit the container
			cmtOpts := docker.CommitContainerOptions{
				Container: container.ID,
			}

			b.Conf.Logger.Debugf("Commiting the container %s", container.ID)
			img, err := b.docker.CommitContainer(cmtOpts)
			if err != nil {
				return err
			}

			b.Conf.Logger.Debugf("Stopping the container %s", container.ID)
			err = b.docker.StopContainer(container.ID, 0)
			if err != nil {
				return err
			}

			tmpFile, err := ioutil.TempFile("", "habitus-export-")
			if err != nil {
				return err
			}
			defer tmpFile.Close()
			tarWriter, err := os.Create(tmpFile.Name())
			if err != nil {
				return err
			}
			defer tarWriter.Close()
			// save the container
			expOpts := docker.ExportImageOptions{
				Name:         img.ID,
				OutputStream: tarWriter,
			}

			b.Conf.Logger.Noticef("Exporting cleaned up container %s to %s", img.ID, tmpFile.Name())
			err = b.docker.ExportImage(expOpts)
			if err != nil {
				return err
			}

			// Squash
			sqTmpFile, err := ioutil.TempFile("", "habitus-export-")
			if err != nil {
				return err
			}
			defer sqTmpFile.Close()
			b.Conf.Logger.Noticef("Squashing image %s into %s", sqTmpFile.Name(), img.ID)

			squasher := squash.Squasher{Conf: b.Conf}
			err = squasher.Squash(tmpFile.Name(), sqTmpFile.Name(), b.uniqueStepName(step))
			if err != nil {
				return err
			}

			b.Conf.Logger.Debugf("Removing exported temp files")
			err = os.Remove(tmpFile.Name())
			if err != nil {
				return err
			}
			// Load
			sqashedFile, err := os.Open(sqTmpFile.Name())
			if err != nil {
				return err
			}
			defer sqashedFile.Close()

			loadOps := docker.LoadImageOptions{
				InputStream: sqashedFile,
			}
			b.Conf.Logger.Debugf("Loading squashed image into docker")
			err = b.docker.LoadImage(loadOps)
			if err != nil {
				return err
			}

			err = os.Remove(sqTmpFile.Name())
			if err != nil {
				return err
			}
		}

		if len(step.Artifacts) > 0 {
			b.Conf.Logger.Noticef("Starting container %s to fetch artifact permissions", container.ID)
			startOpts := &docker.HostConfig{}
			err := b.docker.StartContainer(container.ID, startOpts)
			if err != nil {
				return err
			}

			permMap := make(map[string]int)

			for _, art := range step.Artifacts {
				execOpts := docker.CreateExecOptions{
					Container:    container.ID,
					AttachStdin:  false,
					AttachStdout: true,
					AttachStderr: true,
					Tty:          false,
					Cmd:          []string{"stat", "--format='%a'", art.Source},
				}
				execObj, err := b.docker.CreateExec(execOpts)
				if err != nil {
					return err
				}

				buf := new(bytes.Buffer)
				startExecOpts := docker.StartExecOptions{
					OutputStream: buf,
					ErrorStream:  os.Stderr,
					RawTerminal:  false,
					Detach:       false,
				}

				if err := b.docker.StartExec(execObj.ID, startExecOpts); err != nil {
					b.Conf.Logger.Errorf("Failed to fetch artifact permissions for %s: %s", art.Source, err.Error())
				}

				permsString := strings.Replace(strings.Replace(buf.String(), "'", "", -1), "\n", "", -1)
				perms, err := strconv.Atoi(permsString)
				if err != nil {
					b.Conf.Logger.Errorf("Failed to fetch artifact permissions for %s: %s", art.Source, err.Error())
				}
				permMap[art.Source] = perms
				b.Conf.Logger.Debugf("Permissions for %s is %d", art.Source, perms)
			}

			b.Conf.Logger.Debugf("Stopping the container %s", container.ID)
			err = b.docker.StopContainer(container.ID, 0)
			if err != nil {
				return err
			}

			b.Conf.Logger.Noticef("Copying artifacts from %s", container.ID)

			for _, art := range step.Artifacts {
				err = b.copyToHost(&art, container.ID, permMap)
				if err != nil {
					return err
				}
			}
		}

		// any commands to run?
		if step.Command != "" {
			b.Conf.Logger.Noticef("Starting container %s to run commands", container.ID)
			startOpts := &docker.HostConfig{}

			err := b.docker.StartContainer(container.ID, startOpts)
			if err != nil {
				return err
			}

			execOpts := docker.CreateExecOptions{
				Container:    container.ID,
				AttachStdin:  false,
				AttachStdout: true,
				AttachStderr: true,
				Tty:          true,
				Cmd:          strings.Split(step.Command, " "),
			}
			execObj, err := b.docker.CreateExec(execOpts)
			if err != nil {
				return err
			}

			buf := new(bytes.Buffer)
			startExecOpts := docker.StartExecOptions{
				OutputStream: buf,
				ErrorStream:  os.Stderr,
				RawTerminal:  true,
				Detach:       false,
			}

			b.Conf.Logger.Noticef("Running command %s on container %s", execOpts.Cmd, container.ID)

			if err := b.docker.StartExec(execObj.ID, startExecOpts); err != nil {
				b.Conf.Logger.Errorf("Failed to execute command '%s' due to %s", step.Command, err.Error())
			}

			b.Conf.Logger.Noticef("\n%s", buf)

			inspect, err := b.docker.InspectExec(execObj.ID)
			if err != nil {
				return err
			}

			if inspect.ExitCode != 0 {
				b.Conf.Logger.Errorf("Running command %s on container %s exit with exit code %d", execOpts.Cmd, container.ID, inspect.ExitCode)
				return err
			} else {
				b.Conf.Logger.Noticef("Running command %s on container %s exit with exit code %d", execOpts.Cmd, container.ID, inspect.ExitCode)
			}

			b.Conf.Logger.Debugf("Stopping the container %s", container.ID)
			err = b.docker.StopContainer(container.ID, 0)
			if err != nil {
				return err
			}
		}

		// remove the created container
		removeOpts := docker.RemoveContainerOptions{
			ID:            container.ID,
			RemoveVolumes: true,
			Force:         true,
		}

		b.Conf.Logger.Debugf("Removing built container %s", container.ID)
		err = b.docker.RemoveContainer(removeOpts)
		if err != nil {
			return err
		}
	}

	// clean up the parsed docker file. It will remain there if there was a problem
	err = os.Remove(b.uniqueDockerfile(step))
	if err != nil {
		return err
	}

	return nil
}

// this replaces the FROM field in the Dockerfile to one with the previous step's unique name
// it stores the parsed result Dockefile in uniqueSessionName file
func (b *Builder) replaceFromField(step *Step) error {
	b.Conf.Logger.Noticef("Parsing and converting '%s'", step.Dockerfile)

	rwc, err := os.Open(path.Join(b.Conf.Workdir, step.Dockerfile))
	if err != nil {
		return err
	}
	defer rwc.Close()

	d := parser.Directive{LookingForDirectives: true}
	parser.SetEscapeToken(parser.DefaultEscapeToken, &d)
	node, err := parser.Parse(rwc, &d)
	if err != nil {
		return err
	}

	for _, child := range node.Children {
		if child.Value == "from" {
			// found it. is it from anyone we know?
			if child.Next == nil {
				return errors.New("invalid Dockerfile. No valid FROM found")
			}

			imageName := child.Next.Value
			found, err := step.Manifest.FindStepByName(imageName)
			if err != nil {
				return err
			}

			if found != nil {
				child.Next.Value = b.uniqueStepName(found)
			}
		}
	}

	// did it have any effect?
	b.Conf.Logger.Debugf("Writing the new Dockerfile into %s", step.Dockerfile+".generated")
	err = ioutil.WriteFile(b.uniqueDockerfile(step), []byte(dumpDockerfile(node)), 0644)
	if err != nil {
		return err
	}

	return nil
}

func overwrite(mpath string) (*os.File, error) {
	f, err := os.OpenFile(mpath, os.O_RDWR|os.O_TRUNC, 0777)
	if err != nil {
		f, err = os.Create(mpath)
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func (b *Builder) copyToHost(a *Artifact, container string, perms map[string]int) error {
	// create the artifacts distination folder if not there
	destPath := path.Join(b.Conf.Workdir, a.Dest)
	err := os.MkdirAll(destPath, 0777)
	if err != nil {
		return err
	}

	var out bytes.Buffer
	opt := docker.DownloadFromContainerOptions{
		OutputStream: &out,
		Path:         a.Source,
	}

	err = b.docker.DownloadFromContainer(container, opt)
	if err != nil {
		return err
	}

	// create artifact file on the host
	destFile := path.Join(destPath, filepath.Base(a.Source))
	tr := tar.NewReader(&out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeReg:
			b.Conf.Logger.Infof("Copying from %s to %s", a.Source, destFile)

			dest, err := os.Create(destFile)
			if err != nil {
				return err
			}
			defer dest.Close()

			if _, err := io.Copy(dest, tr); err != nil {
				return err
			}
		default:
			return errors.New("Invalid header type")
		}
	}

	b.Conf.Logger.Debugf("Setting file permissions for %s to %d", destFile, perms[a.Source])
	err = os.Chmod(destFile, os.FileMode(perms[a.Source])|0700)
	if err != nil {
		return err
	}

	return nil
}

func (b *Builder) createContainer(step *Step) (*docker.Container, error) {
	config := docker.Config{
		AttachStdout: true,
		AttachStdin:  false,
		AttachStderr: true,
		Image:        b.uniqueStepName(step),
		Cmd:          []string{"/bin/bash"},
		Tty:          true,
	}

	r, _ := regexp.Compile("/?[^a-zA-Z0-9_-]+")
	containerName := r.ReplaceAllString(b.uniqueStepName(step), "-") + "." + uniuri.New()
	opts := docker.CreateContainerOptions{
		Name:   containerName,
		Config: &config,
	}
	container, err := b.docker.CreateContainer(opts)
	if err != nil {
		return nil, err
	}

	return container, nil
}

func dumpDockerfile(node *parser.Node) string {
	str := ""
	str += node.Value

	if len(node.Flags) > 0 {
		str += fmt.Sprintf(" %q", node.Flags)
	}

	for _, n := range node.Children {
		if (n.Value == "cmd") {
			//keep the old cmd
			str += n.Original + "\n"
		} else {
			str += dumpDockerfile(n) + "\n"
		}
	}

	if node.Next != nil {
		for n := node.Next; n != nil; n = n.Next {
			if len(n.Children) > 0 {
				str += " " + dumpDockerfile(n)
			} else {
				str += " " + n.Value
			}
		}
	}

	return strings.TrimSpace(str)
}

func (b *Builder) uniqueDockerfile(step *Step) string {
	return filepath.Join(b.Conf.Workdir, step.Dockerfile) + ".generated"
}
