package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

type requirementChangeMetadata struct {
	new                          bool
	name, newVersion, oldVersion string
}

type notifier struct {
	*fsnotify.Watcher
	path string
}

func (n *notifier) attachNewFileFileNotifier() error {
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
	fileNotifier := &notifier{path: path}

	err := fileNotifier.attachNewFileFileNotifier()

	if err != nil {
		return nil, err
	}

	return fileNotifier, nil
}

func pushNewRequirementsToGit(username, password, updateMessage, requirementsPath string) error {
	repository, err := git.PlainOpen("./")

	if err != nil {
		return err
	}

	workTree, err := repository.Worktree()

	if err != nil {
		return err
	}

	workTree.Add(requirementsPath)

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

func handleWatchAndNotifyRequirements(requirementsPath string) error {
	// Clean the path to maintain a uniform format for later comparison
	requirementsPath = filepath.Clean(requirementsPath)

	requirementsNotifier, err := newNotifier(requirementsPath)

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
			case <-time.After(time.Second * 6):
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
						requirementsNotifier.Errors <- err
					}

					return
				}
			}
		}
	}

	for {
		select {
		case event := <-requirementsNotifier.Watcher.Events:
			if event.Op == fsnotify.Write && filepath.Clean(event.Name) == requirementsPath {
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
		case err, ok := <-requirementsNotifier.Watcher.Errors:
			if !ok {
				requirementsNotifier.attachNewFileFileNotifier()
			} else if err != nil {
				fmt.Fprintln(os.Stdout, "Error Occured: ", err)
			}
		}
	}
}

func getRequirementsListFromReader(requirementsReader io.Reader) ([]string, error) {
	requirementsLines := []string{}
	lineScanner := bufio.NewScanner(requirementsReader)

	for lineScanner.Scan() {
		requirementsLines = append(requirementsLines, lineScanner.Text())
	}

	return requirementsLines, lineScanner.Err()
}

func getRequirementsListFromPath(requirementsPath string) ([]string, error) {
	requirementsFile, err := os.Open(requirementsPath)

	if err != nil {
		return nil, err
	}

	defer requirementsFile.Close()

	return getRequirementsListFromReader(requirementsFile)
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

func getRequirementsChangeMessage(newRequirements, oldRequirements []string) string {
	name, version := "", ""
	changedRequirements, addedRequirements, removedRequirments := []string{}, []string{}, []string{}
	requirementSplit := []string{}
	newRequirementsMap, oldRequirementsMap := make(map[string]string), make(map[string]string)

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

	fmt.Println(addedRequirements, changedRequirements, removedRequirments)

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

func main() {
	handleRequirements := flag.Bool("hr", false, "Handle when a requests file changes; cannot be used in conjuction with anything other than '-rp'")
	requirementsPath := flag.String("rp", "./requirements.txt", "The path to the requirements file that should be watched or updated")

	flag.Parse()

	oldRequirements, err := getRequirementsListFromPath(*requirementsPath)

	if err != nil {
		panic(err)
	}

	if *handleRequirements {
		if _, err = os.Stat("./config.json"); os.IsNotExist(err) {
			requirementsChangeHandler = func() error {
				return nil
			}

			fmt.Println("No config found, using dummy handler")
		} else {
			credentials := struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}{}

			credentialsFile, err := os.Open("./config.json")

			if err != nil {
				panic(err)
			}

			err = json.NewDecoder(credentialsFile).Decode(&credentials)

			credentialsFile.Close()

			if err != nil {
				panic(err)
			}

			requirementsChangeHandler = func() error {
				newRequirements, err := getRequirementsListFromPath(*requirementsPath)

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
		}

		err = handleWatchAndNotifyRequirements(*requirementsPath)

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

			newRequirements, err = getRequirementsListFromReader(buf)

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
