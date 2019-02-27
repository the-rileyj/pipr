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
	Username string                    `json:"username"`
	Password string                    `json:"password"`
	Parsers  map[string]DependencyFile `json:"parsers"`
}

type DependencyFile struct {
	Command   Command `json:"command"`
	LineMatch string  `json:"line-match"`
}

type Command struct {
	Name        string `json:"name"`
	UpdateMatch string `json:"update-match"`
}

type requirementChangeMetadata struct {
	new                          bool
	name, newVersion, oldVersion string
}

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

func pushNewRequirementsToGit(username, password, updateMessage, filePath string) error {
	repository, err := git.PlainOpen("./")

	if err != nil {
		return err
	}

	workTree, err := repository.Worktree()

	if err != nil {
		return err
	}

	absRequirementsPath, err := filepath.Abs(filePath)

	if err != nil {
		return err
	}

	workTree.Add(absRequirementsPath)

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

func handleWatchAndNotifyRequirements(dependenciesPath string, waitPeriod time.Duration, fileChecker func(string) bool, changeHandler func(string) error) error {
	// Clean the path to maintain a uniform format for later comparison
	dependenciesPath = filepath.Clean(dependenciesPath)

	dependenciesNotifier, err := newNotifier(dependenciesPath)

	if err != nil {
		panic(err)
	}

	// These is nessesary for combatting notifications which happen too
	// frequently; makes so that the handler for the write notifications
	// on the requirements file is only triggered every 2 minutes max
	newNotificationChan := make(chan bool)
	waitingForNewNotifications := false
	handlingLock := &sync.Mutex{}

	waitForNewNotifications := func() {
		for {
			select {
			case <-newNotificationChan:
				continue
			// This resets to waiting for two minutes each iteration
			// of the loop, thus we don't need to worry about adding
			// onto the time to wait after each new notification
			case <-time.After(waitPeriod):
				handlingLock.Lock()

				select {
				// Make sure that a new notification did not occur
				// in between the parent select case and the aquisition
				// of the lock
				case <-newNotificationChan:
					handlingLock.Unlock()

					continue
				default:
					err := requirementsChangeHandler()

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
			if event.Op == fsnotify.Write && filepath.Clean(event.Name) == dependenciesPath {
				handlingLock.Lock()

				// Only check and set waitingForNewNotifications
				// in the lock to prevent race conditions in the
				// waitForNewNotifications goroutine
				if !waitingForNewNotifications {
					go waitForNewNotifications()

					// Indicate that we are already waiting for new
					// notificatations
					waitingForNewNotifications = true
				} else {
					// Send inside of the lock, so that if the
					// waitForNewNotifications goroutine tries
					// to aquire the lock after the wait times
					// out it will continue waiting
					newNotificationChan <- true
				}

				handlingLock.Unlock()
			}
		case err, ok := <-dependenciesNotifier.Watcher.Errors:
			if !ok {
				dependenciesNotifier.attachNewNotifier()
			} else if err != nil {
				fmt.Fprintln(os.Stdout, "Error Occured: ", err)
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

func requirementsChanged(newRequirements, oldRequirements []string) bool {
	if len(newRequirements) != len(oldRequirements) {
		return true
	}

	name, version := "", ""
	requirementSplit := []string{}
	oldRequirementsMap := make(map[string]string)

	for _, oldRequirement := range oldRequirements {
		requirementSplit = strings.Split(oldRequirement, "==")

		if len(requirementSplit) != 2 {
			continue
		}

		name, version = requirementSplit[0], requirementSplit[1]

		oldRequirementsMap[name] = version
	}

	for _, newRequirement := range newRequirements {
		requirementSplit = strings.Split(newRequirement, "==")

		if len(requirementSplit) != 2 {
			continue
		}

		name, version = requirementSplit[0], requirementSplit[1]

		if oldVersion, exists := oldRequirementsMap[name]; !exists || oldVersion != version {
			return true
		}
	}

	return false
}

func getUnversionedDependenciesChangeMessage(newDependencies, oldDependencies []string) string {
	oldDependenciesMap, newDependenciesMap := make(map[string]bool), make(map[string]bool)
	addedDependencies, removedDependencies := []string{}, []string{}

	for _, dependency := range oldDependencies {
		oldDependenciesMap[dependency] = true
	}

	for _, dependency := range newDependencies {
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
		addedRequirementsString, removedRequirmentsString := "", ""

		if len(addedDependencies) != 0 {
			addedRequirementsString = fmt.Sprintf(" Added %s.", strings.Join(addedDependencies, ", "))
		}

		if len(removedDependencies) != 0 {
			removedRequirmentsString = fmt.Sprintf(" Removed %s.", strings.Join(removedDependencies, ", "))
		}

		return fmt.Sprintf(
			"Changed requirements.txt:%s%s",
			addedRequirementsString,
			removedRequirmentsString,
		)
	}

	return ""
}

func getRequirementsChangeMessage(newRequirements, oldRequirements []string, nameVersionMatch string) string {
	name, version := "", ""
	changedRequirements, addedRequirements, removedRequirments := []string{}, []string{}, []string{}
	requirementSplit := []string{}
	newRequirementsMap, oldRequirementsMap := make(map[string]string), make(map[string]string)

	nameVersionMatcher := regexp.MustCompile(nameVersionMatch)

	for _, oldRequirement := range oldRequirements {
		requirementSplit = nameVersionMatcher.FindAllString(oldRequirement, 2)

		// requirementSplit = strings.Split(oldRequirement, "==")

		if len(requirementSplit) != 2 {
			continue
		}

		name, version = requirementSplit[0], requirementSplit[1]

		oldRequirementsMap[name] = version
	}

	for _, newRequirement := range newRequirements {
		// requirementSplit = strings.Split(newRequirement, "==")
		requirementSplit = nameVersionMatcher.FindAllString(newRequirement, 2)

		if len(requirementSplit) != 2 {
			continue
		}

		name, version = requirementSplit[0], requirementSplit[1]

		newRequirementsMap[name] = version

		if oldRequirementVersion, exists := oldRequirementsMap[name]; !exists {
			addedRequirements = append(addedRequirements, name)
		} else if exists {
			if oldRequirementVersion != version {
				changedRequirements = append(changedRequirements, fmt.Sprintf("%s => %s", name, version))
			}
		}
	}

	for _, oldRequirement := range oldRequirements {
		requirementSplit = strings.Split(oldRequirement, "==")

		if len(requirementSplit) != 2 {
			continue
		}

		name, version = requirementSplit[0], requirementSplit[1]

		if _, exists := newRequirementsMap[name]; !exists {
			removedRequirments = append(removedRequirments, name)
		}
	}

	if len(addedRequirements) != 0 || len(changedRequirements) != 0 || len(removedRequirments) != 0 {
		addedRequirementsString, changedRequirementsString, removedRequirmentsString := "", "", ""

		if len(addedRequirements) != 0 {
			addedRequirementsString = fmt.Sprintf(" Added %s.", strings.Join(addedRequirements, ", "))
		}

		if len(changedRequirements) != 0 {
			changedRequirementsString = fmt.Sprintf(" Changed %s.", strings.Join(changedRequirements, ", "))
		}

		if len(removedRequirments) != 0 {
			removedRequirmentsString = fmt.Sprintf(" Removed %s.", strings.Join(removedRequirments, ", "))
		}

		return fmt.Sprintf(
			"Changed requirements.txt:%s%s%s",
			addedRequirementsString,
			changedRequirementsString,
			removedRequirmentsString,
		)
	}

	return ""
}

func handleAptWrapping() {

}

func handlePipWrapping() {

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

	handleRequirements := flag.Bool("hr", false, "Handle when a dependency file changes; cannot be used in conjuction with anything other than '-cp', -rp', and '-w'")
	configPath := flag.String("cp", filepath.Clean(resolvePath("./config.json")(os.LookupEnv("PIPR_CONFIG"))), "The path to the json config file with the github credentials that should be used")
	requirementsPath := flag.String("rp", filepath.Clean(resolvePath("./requirements.txt")(os.LookupEnv("PIPR_REQUIREMENTS"))), "The path to the requirements file that should be watched or updated")
	requirementsWaitPeriod := flag.Duration("w", 2*time.Minute, "How long pipr should wait after each change to the requirements.txt file before pushing the changes to github")

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

	credentialsFile.Close()

	if err != nil {
		panic(err)
	}

	oldRequirements, err := getDependenciesListFromPath(*requirementsPath)

	if err != nil {
		panic(err)
	}

	if *handleRequirements {
		credentials := Config{}

		credentialsFile, err := os.Open("./config.json")

		if err != nil {
			panic(err)
		}

		err = json.NewDecoder(credentialsFile).Decode(&credentials)

		credentialsFile.Close()

		if err != nil {
			panic(err)
		}

		oldRequirements := make(map[string][]string)

		for dependencyFile, _ := range config.Parsers {
			oldRequirements[dependencyFile], err = getDependenciesListFromPath(filepath.Join(*requirementsPath, dependencyFile))

			if err != nil {
				panic(err)
			}
		}

		fileChecker := func(filePath string) bool {
			_, exists := config.Parsers[filepath.Base(filePath)]

			return exists
		}

		dependencyChangeHandler := func(filePath string) error {
			newDependencies, err := getDependenciesListFromPath(filePath)

			if err != nil {
				return err
			}

			commitMessage := getRequirementsChangeMessage(newRequirements, oldRequirements)

			if commitMessage != "" {
				fmt.Println(commitMessage)

				oldRequirements = append([]string{}, newRequirements...)

				return pushNewRequirementsToGit(
					credentials.Username,
					credentials.Password,
					commitMessage,
					*requirementsPath,
				)
			}

			return nil
		}

		err = handleWatchAndNotifyRequirements(*requirementsPath, *requirementsWaitPeriod, fileChecker, dependencyChangeHandler)

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

		// Filter out the requirements path arg and the path itself
		for index := 1; index < len(os.Args); index++ {
			if os.Args[index] == "-rp" {
				index++
			} else {
				args = append(args, os.Args[index])
			}
		}

		// Handles cases where no args are provided, we would get an out of bounds
		// error when checking for the install subcommand without adding this -h
		// (which is the default for pip anyways)
		if len(args) == 0 {
			args = append(args, "-h")
		}

		// Handles checking for "install" and "uninstall"
		update := strings.Contains(args[0], "install")

		cmd := exec.Command("pip", args...)

		cmd.Stderr, cmd.Stdin, cmd.Stdout = os.Stderr, os.Stdin, os.Stdout

		cmd.Run()

		if update {
			buf := &bytes.Buffer{}
			newRequirements := []string{}

			cmd = exec.Command("pip", "freeze")

			cmd.Stdout = buf

			err = cmd.Run()

			if err != nil {
				panic(err)
			}

			newRequirements, err = getDependenciesListFromReader(buf)

			if err != nil {
				panic(err)
			}

			requirementsFile, err := os.OpenFile(*requirementsPath, os.O_WRONLY, 0444)

			if err != nil {
				panic(err)
			}

			defer requirementsFile.Close()

			if requirementsChanged(newRequirements, oldRequirements) {
				for _, newRequirement := range newRequirements {
					fmt.Fprintln(requirementsFile, newRequirement)
				}
			}
		}
	}
}
