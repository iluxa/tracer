package main

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-errors/errors"
	"github.com/kubeshark/tracer/misc"
	"github.com/rs/zerolog/log"
	v1 "k8s.io/api/core/v1"
)

var numberRegex = regexp.MustCompile("[0-9]+")

func UpdateTargets(pods []v1.Pod) error {
	containerIds := buildContainerIdsMap(pods)
	log.Debug().Interface("container-ids", containerIds).Send()

	containerPids, err := findContainerPids(tracer.procfs, containerIds)
	if err != nil {
		return err
	}

	log.Info().Interface("pids", reflect.ValueOf(containerPids).MapKeys()).Send()

	tracer.ClearPids()

	// TODO: CAUSES INITIAL MEMORY SPIKE
	for pid := range containerPids {
		if err := tracer.AddSSLLibPid(tracer.procfs, pid); err != nil {
			LogError(err)
		}

		if err := tracer.AddGoPid(tracer.procfs, pid); err != nil {
			LogError(err)
		}
	}

	return nil
}

func UpdateTargetsHost(cmdLineRegex *regexp.Regexp) error {
	hostPids, err := findContainerPidsHost(tracer.procfs, cmdLineRegex)
	if err != nil {
		return err
	}

	log.Info().Interface("pids", hostPids).Send()

	tracer.ClearPids()

	// TODO: CAUSES INITIAL MEMORY SPIKE
	for _, pid := range hostPids {
		if err := tracer.AddSSLLibPid(tracer.procfs, pid); err != nil {
			LogError(err)
		}

		if err := tracer.AddGoPid(tracer.procfs, pid); err != nil {
			LogError(err)
		}
	}

	return nil
}

func findContainerPids(procfs string, containerIds map[string]v1.Pod) (map[uint32]v1.Pod, error) {
	result := make(map[uint32]v1.Pod)

	pids, err := os.ReadDir(procfs)
	if err != nil {
		return result, err
	}

	log.Info().Str("procfs", procfs).Int("pids", len(pids)).Msg("Starting TLS auto discoverer:")

	for _, pid := range pids {
		if !pid.IsDir() {
			continue
		}

		if !numberRegex.MatchString(pid.Name()) {
			continue
		}

		cgroup, err := getProcessCgroup(procfs, pid.Name())
		if err != nil {
			log.Warn().Err(err).Str("pid", pid.Name()).Msg("Couldn't get the cgroup of process.")
			continue
		}

		pod, ok := containerIds[cgroup]
		if !ok {
			log.Warn().Str("pid", pid.Name()).Str("cgroup", cgroup).Msg("Couldn't find the pod for the given cgroup of pid.")
			continue
		}

		pidNumber, err := strconv.Atoi(pid.Name())
		if err != nil {
			log.Warn().Str("pid", pid.Name()).Msg("Unable to convert the process id to integer.")
			continue
		}

		result[uint32(pidNumber)] = pod
	}

	return result, nil
}

func findContainerPidsHost(procfs string, cmdLineRegex *regexp.Regexp) ([]uint32, error) {
	var result []uint32

	pids, err := os.ReadDir(procfs)
	if err != nil {
		return result, err
	}

	log.Info().Str("procfs", procfs).Int("pids", len(pids)).Msg("Starting TLS auto discoverer:")

	for _, pid := range pids {
		if !pid.IsDir() {
			continue
		}

		if !numberRegex.MatchString(pid.Name()) {
			continue
		}

		pidNumber, err := strconv.Atoi(pid.Name())
		if err != nil {
			log.Warn().Str("pid", pid.Name()).Msg("Unable to convert the process id to integer.")
			continue
		}

		if cmdLineRegex != nil {
			cmdLine, err := misc.GetProcCmdLine(pid.Name())
			if err != nil {
				log.Warn().Err(err).Str("pid", pid.Name()).Msg("Unable to get pid cmdline.")
				continue
			}

			if !cmdLineRegex.MatchString(cmdLine) {
				continue
			}
		}

		result = append(result, uint32(pidNumber))
	}

	return result, nil
}

func buildContainerIdsMap(pods []v1.Pod) map[string]v1.Pod {
	result := make(map[string]v1.Pod)

	for _, pod := range pods {
		for _, container := range pod.Status.ContainerStatuses {
			parsedUrl, err := url.Parse(container.ContainerID)
			if err != nil {
				log.Warn().Msg(fmt.Sprintf("Expecting URL like container ID %v", container.ContainerID))
				continue
			}

			result[parsedUrl.Host] = pod
		}
	}

	return result
}

func getProcessCgroup(procfs string, pid string) (string, error) {
	fpath := fmt.Sprintf("%s/%s/cgroup", procfs, pid)

	bytes, err := os.ReadFile(fpath)
	if err != nil {
		return "", errors.Errorf("Error reading cgroup file %s - %v", fpath, err)
	}

	lines := strings.Split(string(bytes), "\n")
	cgrouppath := extractCgroup(lines)

	if strings.Contains(cgrouppath, "-") {
		parts := strings.Split(cgrouppath, "-")
		cgrouppath = parts[len(parts)-1]
	}

	if cgrouppath == "" {
		return "", errors.Errorf("Cgroup path not found for %s, %s", pid, lines)
	}

	return normalizeCgroup(cgrouppath), nil
}

func extractCgroup(lines []string) string {
	if len(lines) == 1 {
		parts := strings.Split(lines[0], ":")
		return parts[len(parts)-1]
	} else {
		for _, line := range lines {
			if strings.Contains(line, ":pids:") {
				parts := strings.Split(line, ":")
				return parts[len(parts)-1]
			}
		}
	}

	return ""
}

// cgroup in the /proc/<pid>/cgroup may look something like
//
//  /system.slice/docker-<ID>.scope
//  /system.slice/containerd-<ID>.scope
//  /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod3beae8e0_164d_4689_a087_efd902d8c2ab.slice/docker-<ID>.scope
//  /kubepods/besteffort/pod7709c1d5-447c-428f-bed9-8ddec35c93f4/<ID>
//
// This function extract the <ID> out of the cgroup path, the <ID> should match
//	the "Container ID:" field when running kubectl describe pod <POD>
//
func normalizeCgroup(cgrouppath string) string {
	basename := strings.TrimSpace(path.Base(cgrouppath))

	if strings.Contains(basename, "-") {
		basename = basename[strings.Index(basename, "-")+1:]
	}

	if strings.Contains(basename, ".") {
		return strings.TrimSuffix(basename, filepath.Ext(basename))
	} else {
		return basename
	}
}
