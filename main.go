package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/Masterminds/semver"
	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/labstack/echo"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

func main() {

	// initialize web server
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		if !c.Response().Committed {
			if c.Request().Method == "HEAD" {
				err = c.NoContent(
					http.StatusInternalServerError,
				)
			} else {
				err = c.JSONPretty(
					http.StatusInternalServerError,
					map[string]string{
						"error": err.Error(),
					},
					"  ",
				)
			}
			if err != nil {
				logrus.Errorln(err)
			}
		}
	}

	v1 := e.Group("/api/v1")
	updGroup := v1.Group("/update")
	updGroup.GET("", updManual)
	updGroup.POST("", updByHook)

	// http probe
	e.GET("/probe", probe)

	address := ":8084"
	logrus.Infof("starting docker-updater API server on %s", address)
	logrus.Fatal(e.Start(address))

}

func probe(c echo.Context) error {
	return c.String(http.StatusOK, "OK")
}

// testing update call: GET /api/v1/update?repo=REPO&tag=TAG
func updManual(c echo.Context) error {
	return _upd(c, c.QueryParam("repo"), c.QueryParam("tag"))
}

// prod update call: POST /api/v1/update
func updByHook(c echo.Context) error {
	var p push
	if err := c.Bind(&p); err != nil {
		return err
	}
	return _upd(c, p.Repository.RepoName, p.Data.Tag)
}

func _upd(c echo.Context, repo, tag string) error {
	if err := updateContainer(repo, tag); err != nil {
		return err
	} else {
		return c.String(http.StatusOK, "OK")
	}
}

// ======= STRUCTURES ======

// docker hub hook payload
type push struct {
	Data       pushData   `json:"push_data"`
	Repository repository `json:"repository"`
}
type pushData struct {
	PushedAt int64  `json:"pushed_at"`
	Tag      string `json:"tag"`
	Pusher   string `json:"pusher"`
}
type repository struct {
	RepoName  string `json:"repo_name"`
	IsTrusted bool   `json:"is_trusted"`
}

// ======= ACTIONS ======

var cli *client.Client
var ctx context.Context

const latest = "latest"

func init() {
	var err error
	cli, err = client.NewEnvClient()
	if err != nil {
		logrus.Panicf("unable to init docker client: %s", err.Error())
	}
	ctx = context.Background()
}

func updateContainer(repo, tag string) error {

	defer func() {
		logrus.Infof("===========")
	}()

	if repo == "" || tag == "" {
		return _err("repo and tag must be filled")
	}

	var fullRepo = fmt.Sprintf("%s:%s", repo, tag)
	logrus.Infof("updating repo %s...", fullRepo)
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		return _err("get containers list error: %s", err.Error())
	}

	var toUpdate []types.Container
	var containerImages []string
	for _, cnt := range containers {
		containerImages = append(containerImages, cnt.Image)
		iParts := strings.Split(cnt.Image, ":")
		var cRepo, cTag = iParts[0], ""
		if len(iParts) > 1 {
			cTag = iParts[1]
		}
		if cTag == "" {
			cTag = latest
		}
		if cRepo == repo {
			var upd bool
			var cVer, ver *semver.Version
			var vErr error
			if cTag == latest {
				upd = tag == cTag
			} else {
				if cVer, vErr = semver.NewVersion(cTag); vErr != nil {
					logrus.Errorf("error parsing existing container tag %s: %s", cTag, vErr)
					continue
				}
				if ver, vErr = semver.NewVersion(tag); vErr != nil {
					logrus.Errorf("error parsing existing container tag %s: %s", tag, vErr)
					continue
				}
				upd =
					cVer.Prerelease() == ver.Prerelease() &&
						cVer.Metadata() == ver.Metadata() &&
						cVer.LessThan(ver)
			}
			if upd {
				c := cnt
				toUpdate = append(toUpdate, c)
				logrus.Infof("to update %s:%s -> %s", cRepo, cVer.String(), ver.String())
			}
		}
	}
	if len(containerImages) > 0 {
		logrus.Infof("existing containers images: %s", strings.Join(containerImages, ", "))
	}
	if len(toUpdate) == 0 {
		logrus.Infof("no containers should be updated with image %s found, skipped", fullRepo)
		return nil
	}

	pn, err := reference.ParseNormalizedNamed(fullRepo)
	if err != nil {
		return _err("parse container name %s error: %s", fullRepo, err.Error())
	}
	logrus.Infof("pulling repo %s...", fullRepo)
	pullStart := time.Now()
	out, err := cli.ImagePull(ctx, pn.String(), types.ImagePullOptions{})
	if err != nil {
		return _err("pull image %s error: %s", fullRepo, err.Error())
	}
	defer func() {
		if err := out.Close(); err != nil {
			logrus.Errorf("error closing image pooling: %s", err)
		}
	}()
	_, _ = io.Copy(ioutil.Discard, out)
	logrus.Infof("repo %s pulled for %v", fullRepo, time.Since(pullStart))

	logrus.Infof("restarting %d containers...", len(toUpdate))
	for _, cnt := range toUpdate {
		inspect, err := cli.ContainerInspect(ctx, cnt.ID)
		if err != nil {
			return _err("inspect container %s error: %s", cnt.ID, err.Error())
		}
		prevImageId := inspect.Image
		if err = cli.ContainerRemove(ctx, cnt.ID, types.ContainerRemoveOptions{Force: true}); err != nil {
			return _err("remove container %s error: %s", cnt.ID, err.Error())
		}
		var contConfig *container.Config
		if inspect.Config != nil {
			contConfig = inspect.Config
		}
		contConfig.Image = strings.TrimSuffix(fullRepo, ":"+latest)

		var networkingConfig *network.NetworkingConfig
		if inspect.NetworkSettings != nil && inspect.NetworkSettings.Networks != nil {
			networkingConfig = &network.NetworkingConfig{
				EndpointsConfig: inspect.NetworkSettings.Networks,
			}
		}

		created, err := cli.ContainerCreate(ctx, contConfig, inspect.HostConfig, networkingConfig, inspect.Name)
		if err != nil {
			return _err("create new container error: %s", err.Error())
		}
		if err := cli.ContainerStart(ctx, created.ID, types.ContainerStartOptions{}); err != nil {
			return _err("start new container error: %s", err.Error())
		}

		inspect, err = cli.ContainerInspect(ctx, created.ID)
		if prevImageId != inspect.Image {
			logrus.Infof("clearing previous not actual images for %s...", fullRepo)
			rm, err := cli.ImageRemove(ctx, prevImageId, types.ImageRemoveOptions{})
			if err != nil {
				logrus.Errorf("remove previous image error: %s", err)
			} else {
				for _, rmi := range rm {
					if rmi.Untagged != "" {
						logrus.Infof(" - untagged: %s", rmi.Untagged)
					}
					if rmi.Deleted != "" {
						logrus.Infof(" - deleted: %s", rmi.Deleted)
					}
				}
			}
		}

	}

	logrus.Infof("updating containers for repo %s done!", fullRepo)
	return nil

}

func _err(format string, args ...interface{}) error {
	var msg = fmt.Sprintf(format, args...)
	return errors.New(msg)
}
