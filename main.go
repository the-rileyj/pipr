package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	git "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
)

var (
	requirementsChangeHandler func() error
)

type Config struct {
	Username     string        `json:"username"`
	Password     string        `json:"password"`
	Dependencies []*Dependency `json:"dependencies"`
}

type Dependency struct {
	FileName           string   `json:"file-name"`
	Command            string   `json:"command"`
	LineMatch          string   `json:"line-match"`
	UpdateDependencies []string `json:"update-dependencies"`
	UpdateMatch        string   `json:"update-match"`
	Versioned          bool     `json:"versioned"`
}

// type Config struct {
// 	Username string                    `json:"username"`
// 	Password string                    `json:"password"`
// 	Parsers  map[string]DependencyFile `json:"parsers"`
// }

// type DependencyFile struct {
// 	Command   Command `json:"command"`
// 	LineMatch string  `json:"line-match"`
// }

// type Command struct {
// 	Name        string `json:"name"`
// 	UpdateMatch string `json:"update-match"`
// }

// type requirementChangeMetadata struct {
// 	new                          bool
// 	name, newVersion, oldVersion string
// }

type notifier struct {
	*fsnotify.Watcher
	path string
}

func (n *notifier) attachNewNotifier() error {
	requirementWatcher, err := fsnotify.NewWatcher()

	if err != nil {
		return err
	}

	err = requirementWatcher.Add(n.path)

	if err != nil {
		return err
	}

	n.Watcher = requirementWatcher

	return nil
}

func newNotifier(path string) (*notifier, error) {
	newNotifier := &notifier{path: path}

	err := newNotifier.attachNewNotifier()

	if err != nil {
		return nil, err
	}

	return newNotifier, nil
}

func pushNewRequirementsToGit(username, password, updateMessage string, filePaths []string) error {
	repository, err := git.PlainOpen("./")

	if err != nil {
		return err
	}

	workTree, err := repository.Worktree()

	if err != nil {
		return err
	}

	for _, filePath := range filePaths {

		absRequirementsPath, err := filepath.Abs(filePath)

		if err != nil {
			return err
		}

		workTree.Add(absRequirementsPath)
	}

	workTree.Commit(updateMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Riley Johnson",
			Email: "rj@therileyjohnson.com",
			When:  time.Now(),
		},
	})

	err = repository.Push(&git.PushOptions{
		Auth: &http.BasicAuth{
			Username: username,
			Password: password,
		},
	})

	return err
}

func handleWatchAndNotifyRequirements(dependenciesPath string, waitPeriod time.Duration, fileChecker func(string) bool, changeHandler func([]string) error) error {
	// Clean the path to maintain a uniform format for later comparison
	dependenciesPath = filepath.Clean(dependenciesPath)

	dependenciesNotifier, err := newNotifier(dependenciesPath)

	if err != nil {
		panic(err)
	}

	// These is necessary for combatting notifications which happen too
	// frequently; makes so that the handler for the write notifications
	// on the requirements file is only triggered every 2 minutes max
	newNotificationChan := make(chan string)
	waitingForNewNotifications := false
	handlingLock := &sync.Mutex{}

	waitForNewNotifications := func(newUpdateFile string) {
		updateFilesMap := map[string]bool{
			newUpdateFile: true,
		}

		for {
			select {
			case newUpdateFile = <-newNotificationChan:
				if !updateFilesMap[newUpdateFile] {
					updateFilesMap[newUpdateFile] = true
				}

				continue
			// This resets to waiting for two minutes each iteration
			// of the loop, thus we don't need to worry about adding
			// onto the time to wait after each new notification
			case <-time.After(waitPeriod):
				handlingLock.Lock()

				select {
				// Make sure that a new notification did not occur
				// in between the parent select case and the acquisition
				// of the lock
				case <-newNotificationChan:
					handlingLock.Unlock()

					continue
				default:
					updateFilesList := make([]string, 0)

					for updateFile := range updateFilesMap {
						updateFilesList = append(updateFilesList, updateFile)
					}

					err := changeHandler(updateFilesList)

					// Indicate that we are no longer listening
					// for new notifications
					waitingForNewNotifications = false

					// Unlock before sending the error to prevent
					// starvation in the case that the error channel
					// requirementsNotifier.Watcher.Errors in the
					// goroutine that spawned this one is already full
					// and is trying to requesting the lock (preventing
					// it from being able to handle the error being sent)
					handlingLock.Unlock()

					if err != nil {
						// Reuse the existing error channel on the
						// watcher for sending errors
						dependenciesNotifier.Errors <- err
					}

					return
				}
			}
		}
	}

	for {
		select {
		case event := <-dependenciesNotifier.Watcher.Events:
			if event.Op == fsnotify.Write && fileChecker(event.Name) {
				handlingLock.Lock()

				// Only check and set waitingForNewNotifications
				// in the lock to prevent race conditions in the
				// waitForNewNotifications goroutine
				if !waitingForNewNotifications {
					go waitForNewNotifications(event.Name)

					// Indicate that we are already waiting for new
					// notifications
					waitingForNewNotifications = true
				} else {
					// Send inside of the lock, so that if the
					// waitForNewNotifications goroutine tries
					// to acquire the lock after the wait times
					// out it will continue waiting
					newNotificationChan <- event.Name
				}

				handlingLock.Unlock()
			}
		case err, ok := <-dependenciesNotifier.Watcher.Errors:
			if !ok {
				dependenciesNotifier.attachNewNotifier()
			} else if err != nil {
				fmt.Fprintln(os.Stdout, "Error Occurred: ", err)
			}
		}
	}
}

func getDependenciesListFromReader(requirementsReader io.Reader) ([]string, error) {
	requirementsLines := []string{}
	lineScanner := bufio.NewScanner(requirementsReader)

	for lineScanner.Scan() {
		requirementsLines = append(requirementsLines, lineScanner.Text())
	}

	return requirementsLines, lineScanner.Err()
}

func getDependenciesListFromPath(packagesPath string) ([]string, error) {
	requirementsFile, err := os.Open(packagesPath)

	if err != nil {
		return nil, err
	}

	defer requirementsFile.Close()

	return getDependenciesListFromReader(requirementsFile)
}

func dependenciesChanged(newDependencies, oldDependencies []string) bool {
	if len(newDependencies) != len(oldDependencies) {
		return true
	}

	oldRequirementsMap := make(map[string]bool)

	for _, oldRequirement := range oldDependencies {
		oldRequirementsMap[oldRequirement] = true
	}

	for _, newRequirement := range newDependencies {
		if !oldRequirementsMap[newRequirement] {
			return true
		}
	}

	return false
}

func getUnversionedDependenciesChangeMessage(newDependencies, oldDependencies []string, nameMatch string) string {
	oldDependenciesMap, newDependenciesMap := make(map[string]bool), make(map[string]bool)
	addedDependencies, removedDependencies := []string{}, []string{}
	name := []string{}

	nameMatcher := regexp.MustCompile(nameMatch)

	for _, dependency := range oldDependencies {
		name = nameMatcher.FindStringSubmatch(dependency)

		if len(name) != 2 {
			continue
		}

		dependency = name[1]

		oldDependenciesMap[dependency] = true
	}

	for _, dependency := range newDependencies {
		name = nameMatcher.FindStringSubmatch(dependency)

		if len(name) != 2 {
			continue
		}

		dependency = name[1]

		if !oldDependenciesMap[dependency] {
			addedDependencies = append(addedDependencies, dependency)
		}

		newDependenciesMap[dependency] = true
	}

	for _, dependency := range oldDependencies {
		if !newDependenciesMap[dependency] {
			removedDependencies = append(removedDependencies, dependency)
		}
	}

	if len(addedDependencies) != 0 || len(removedDependencies) != 0 {
		addedDependenciesString, removedDependenciesString := "", ""

		if len(addedDependencies) != 0 {
			addedDependenciesString = fmt.Sprintf(" Added %s.", strings.Join(addedDependencies, ", "))
		}

		if len(removedDependencies) != 0 {
			removedDependenciesString = fmt.Sprintf(" Removed %s.", strings.Join(removedDependencies, ", "))
		}

		return fmt.Sprintf(
			"Changed %%s:%s%s",
			addedDependenciesString,
			removedDependenciesString,
		)
	}

	return ""
}

func getVersionedDependenciesChangeMessage(newRequirements, oldRequirements []string, nameVersionMatch string) string {
	name, version := "", ""
	changedDependencies, addedDependencies, removedDependencies := []string{}, []string{}, []string{}
	requirementSplit := []string{}
	newRequirementsMap, oldRequirementsMap := make(map[string]string), make(map[string]string)

	nameVersionMatcher := regexp.MustCompile(nameVersionMatch)

	for _, oldRequirement := range oldRequirements {
		requirementSplit = nameVersionMatcher.FindStringSubmatch(oldRequirement)

		if len(requirementSplit) != 3 {
			continue
		}

		name, version = requirementSplit[1], requirementSplit[2]

		oldRequirementsMap[name] = version
	}

	for _, newRequirement := range newRequirements {
		requirementSplit = nameVersionMatcher.FindStringSubmatch(newRequirement)

		if len(requirementSplit) != 3 {
			continue
		}

		name, version = requirementSplit[1], requirementSplit[2]

		newRequirementsMap[name] = version

		if oldRequirementVersion, exists := oldRequirementsMap[name]; !exists {
			addedDependencies = append(addedDependencies, name)
		} else if exists {
			if oldRequirementVersion != version {
				changedDependencies = append(changedDependencies, fmt.Sprintf("%s => %s", name, version))
			}
		}
	}

	for _, oldRequirement := range oldRequirements {
		requirementSplit = nameVersionMatcher.FindStringSubmatch(oldRequirement)

		if len(requirementSplit) != 3 {
			continue
		}

		name, version = requirementSplit[1], requirementSplit[2]

		if _, exists := newRequirementsMap[name]; !exists {
			removedDependencies = append(removedDependencies, name)
		}
	}

	if len(addedDependencies) != 0 || len(changedDependencies) != 0 || len(removedDependencies) != 0 {
		addedDependenciesString, changedDependenciesString, removedDependenciesString := "", "", ""

		if len(addedDependencies) != 0 {
			addedDependenciesString = fmt.Sprintf(" Added %s.", strings.Join(addedDependencies, ", "))
		}

		if len(changedDependencies) != 0 {
			changedDependenciesString = fmt.Sprintf(" Changed %s.", strings.Join(changedDependencies, ", "))
		}

		if len(removedDependencies) != 0 {
			removedDependenciesString = fmt.Sprintf(" Removed %s.", strings.Join(removedDependencies, ", "))
		}

		return fmt.Sprintf(
			"Changed %%s:%s%s%s",
			addedDependenciesString,
			changedDependenciesString,
			removedDependenciesString,
		)
	}

	return ""
}

func main() {
	// Handles finding the correct path to the requirements file
	resolvePath := func(defaultPath string) func(string, bool) string {
		return func(requirementsEnv string, exists bool) string {
			if exists {
				return requirementsEnv
			}

			return defaultPath
		}
	}

	handleRequirements := flag.Bool("hr", false, "Handle when a dependency file changes; cannot be used in conjunction with anything other than '-cp', -rp', and '-w'")
	configPath := flag.String("cp", filepath.Clean(resolvePath("./config.json")(os.LookupEnv("RJIN_CONFIG"))), "The path to the json config file with the github credentials that should be used")
	dependenciesPath := flag.String("dp", filepath.Clean(resolvePath("./dependencies/")(os.LookupEnv("RJIN_DEPENDENCIES"))), "The path to the dependencies directory that should be watched or updated")
	requirementsWaitPeriod := flag.Duration("w", 2*time.Minute, "How long rjin should wait after each change to the requirements.txt file before pushing the changes to github")

	flag.Parse()

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		panic(errors.New("need a config to work from for updating and syncing dependency files"))
	}

	config := Config{}

	configFile, err := os.Open(*configPath)

	if err != nil {
		panic(err)
	}

	err = json.NewDecoder(configFile).Decode(&config)

	configFile.Close()

	if err != nil {
		panic(err)
	}

	commandToDependenciesMap := make(map[string]*Dependency)
	fileToDependenciesMap := make(map[string]*Dependency)
	oldDependenciesMap := make(map[string][]string)

	for _, dependency := range config.Dependencies {
		oldDependenciesMap[dependency.FileName], err = getDependenciesListFromPath(filepath.Join(*dependenciesPath, dependency.FileName))

		if err != nil {
			panic(fmt.Errorf("failed to get existing dependencies for %s file", dependency.FileName))
		}

		commandToDependenciesMap[dependency.Command] = dependency
		fileToDependenciesMap[dependency.FileName] = dependency
	}

	if *handleRequirements {
		fileChecker := func(filePath string) bool {
			_, exists := oldDependenciesMap[filepath.Base(filePath)]

			return exists
		}

		dependencyChangeHandler := func(filePaths []string) error {
			var commitMessage string
			commitFiles, commitMessages := []string{}, []string{}

			for _, filePath := range filePaths {
				dependency := fileToDependenciesMap[filepath.Base(filePath)]
				newDependencies, err := getDependenciesListFromPath(filePath)

				if err != nil {
					return err
				}

				if dependency.Versioned {
					commitMessage = getVersionedDependenciesChangeMessage(
						newDependencies,
						oldDependenciesMap[filepath.Base(filePath)],
						dependency.LineMatch,
					)
				} else {
					commitMessage = getUnversionedDependenciesChangeMessage(
						newDependencies,
						oldDependenciesMap[filepath.Base(filePath)],
						dependency.LineMatch,
					)
				}

				if commitMessage != "" {
					oldDependenciesMap[filepath.Base(filePath)] = newDependencies

					commitMessages = append(commitMessages, fmt.Sprintf(commitMessage, filepath.Base(filePath)))
					commitFiles = append(commitFiles, filepath.Join(*dependenciesPath, dependency.FileName))
				}
			}

			if len(commitMessages) != 0 {
				return pushNewRequirementsToGit(
					config.Username,
					config.Password,
					strings.Join(commitMessages, " "),
					commitFiles,
				)
			}

			return nil
		}

		err = handleWatchAndNotifyRequirements(*dependenciesPath, *requirementsWaitPeriod, fileChecker, dependencyChangeHandler)

		if err != nil {
			panic(err)
		}

	} else {
		// Default behavior is to read the requirements file as a list of strings,
		// then pass the arguments as is into pip and run it the command, and if the
		// any of the args to pip are anything that would change the requirements file
		// (ex. install, uninstall) compare the string list representing the original
		// requirements file to the result of the "pip freeze" command to see if
		// anything has changed
		args := []string{}

		// Filter out the dependencies directory path arg and the path itself
		for index := 1; index < len(os.Args); index++ {
			if os.Args[index] == "-dp" {
				index++
			} else {
				args = append(args, os.Args[index])
			}
		}

		// Handles cases where no args are provided, we would get an out of bounds
		// error when checking for the install subcommand without adding this
		if len(args) == 0 {
			panic(errors.New("no command provided, need a command to execute correctly"))
		}

		dependency, exists := commandToDependenciesMap[args[0]]

		if !exists {
			panic(errors.New("command provided does not exist in the config, need a command which exists in the config to execute correctly"))
		}

		// Handles when there is no way to update the dependencies after the command executes
		if len(dependency.UpdateDependencies) == 0 {
			panic(errors.New("command provided does not have any way for updating it's dependencies in the config, need a way of updating dependencies afterwards"))
		}

		command := args[0]

		args = append([]string{}, args[1:]...)

		var update bool

		if len(args) != 0 {
			// Handles checking for a subcommand which would change dependency files;
			// note, it only checks the first subcommand, ex. "install" in "pip install"
			update, _ = regexp.Match(dependency.UpdateMatch, []byte(args[0]))
		}

		cmd := exec.Command(command, args...)

		cmd.Stderr, cmd.Stdin, cmd.Stdout = os.Stderr, os.Stdin, os.Stdout

		cmd.Run()

		if update {
			buf := &bytes.Buffer{}
			newDependencies := []string{}

			cmd = exec.Command(dependency.UpdateDependencies[0], dependency.UpdateDependencies[1:]...)

			cmd.Stdout = buf

			err = cmd.Run()

			if err != nil {
				panic(err)
			}

			newDependencies, err = getDependenciesListFromReader(buf)

			if err != nil {
				panic(err)
			}

			dependencyFile, err := os.OpenFile(filepath.Join(*dependenciesPath, dependency.FileName), os.O_WRONLY, 0444)

			if err != nil {
				panic(err)
			}

			defer dependencyFile.Close()

			if dependenciesChanged(newDependencies, oldDependenciesMap[dependency.FileName]) {
				for _, newDependency := range newDependencies {
					fmt.Fprintln(dependencyFile, newDependency)
				}
			}
		}
	}
}
