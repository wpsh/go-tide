package main

import (
	"github.com/wptide/pkg/sync"
	"github.com/wptide/pkg/message"
	"github.com/wptide/pkg/wporg"
	"time"
	"fmt"
	"strings"
	"github.com/wptide/pkg/env"
	"log"
	"strconv"
)

var (
	// Sync Server config.
	serverActive    = strings.ToLower(env.GetEnv("SYNC_ACTIVE", "off")) == "on"
	forcedSync      = strings.ToLower(env.GetEnv("SYNC_FORCE_AUDITS", "no")) == "yes"
	apiClient       = env.GetEnv("SYNC_DEFAULT_CLIENT", "wporg")
	auditVisibility = env.GetEnv("SYNC_DEFAULT_VISIBILITY", "public")
	perPage, _      = strconv.Atoi(env.GetEnv("SYNC_ITEMS_PER_PAGE", "250"))
	poolSize, _     = strconv.Atoi(env.GetEnv("SYNC_POOL_WORKERS", "250"))
	browseCategory  = env.GetEnv("SYNC_API_BROWSE_CATEGORY", "updated")
	poolDelay, _    = strconv.Atoi(env.GetEnv("SYNC_POOL_DELAY", "300"))

	// Allow us to quit the server with a channel. Good for tests.
	quit = make(chan struct{}, 1)

	// Fetches themes and plugins from the WordPress.org API.
	client = &wporg.Client{}

	// Checks to see if we need to send an update.
	checkerDBPath = env.GetEnv("SYNC_DATA", "./db")

	// Init the scribble (flat file) db used for checking currency of results.
	checker sync.UpdateChecker = scribbleChecker{
		db: newScribbleChecker(checkerDBPath),
	}

	// Create the SQS dispatcher.
	dispatcher = &sqsDispatcher{
		Queues: map[string]struct {
			Region   string
			Key      string
			Secret   string
			Queue    string
			Endpoint string
			Active   bool
			Accepts  string // "all" or "themes" or "plugins"
		}{
			"phpcs": {
				env.GetEnv("AWS_SQS_REGION", ""),
				env.GetEnv("AWS_API_KEY", ""),
				env.GetEnv("AWS_API_SECRET", ""),
				env.GetEnv("AWS_SQS_QUEUE_PHPCS", ""),
				apiEndpoint,
				strings.ToLower(env.GetEnv("SYNC_PHPCS_ACTIVE", "on")) == "on",
				"all",
			},
			"lighthouse": {
				env.GetEnv("AWS_SQS_REGION", ""),
				env.GetEnv("AWS_API_KEY", ""),
				env.GetEnv("AWS_API_SECRET", ""),
				env.GetEnv("AWS_SQS_QUEUE_LH", ""),
				apiEndpoint,
				strings.ToLower(env.GetEnv("SYNC_LH_ACTIVE", "off")) == "on",
				"themes",
			},
		},
		providers: make(map[string]message.MessageProvider),
	}

	// Tide API settings.
	apiVersion  = env.GetEnv("API_VERSION", "v1")
	apiProtocol = env.GetEnv("API_PROTOCOL", "https")
	apiHost     = env.GetEnv("API_HTTP_HOST", "wptide.org")
	apiEndpoint = fmt.Sprintf("%s://%s/api/tide/%s/audit", apiProtocol, apiHost, apiVersion)
)

// fetcher returns a channel of RepoProjects to be processed by workers. It uses the
// "generator" concurrency pattern for feeding a channel.
func fetcher(projectType, category string, bufferSize int, token chan struct{}, maxPages int) <-chan wporg.RepoProject {

	// The new channel that will be processed by the workers.
	out := make(chan wporg.RepoProject, bufferSize)

	// Feed the channel concurrently.
	go func(token chan struct{}) {

		// Don't do anything until a token is available.
		<-token

		var response *wporg.ApiResponse
		var err error

		for {

			page := 1
			pages := 1

			for page <= pages {

				if projectType == "themes" {
					response, err = client.RequestThemes(category, bufferSize, page)
				} else {
					response, err = client.RequestPlugins(category, bufferSize, page)
				}

				if err != nil {
					log.Println(err)
					page = pages + 1
					continue
				}

				pages = response.Info.Pages
				if maxPages != -1 && pages > maxPages {
					pages = maxPages
				}

				// Themes.
				for _, project := range response.Themes {
					project.Type = projectType
					out <- project
				}

				// Plugins.
				for _, project := range response.Plugins {
					project.Type = projectType
					out <- project
				}

				page += 1
			}

			log.Println(projectType, "sync finished at", time.Now().Format(time.RFC3339))

			// Finished, so pass on the token.
			token <- struct{}{}

			time.Sleep(time.Second * time.Duration(poolDelay))

			// Wait until this process receives a token.
			<-token
		}
	}(token)

	return out
}

// pool starts a number of infoWorkers. This uses a "worker" pattern where each worker will
// read from a projects channel to process the results.
func pool(workers int, projects <-chan wporg.RepoProject, checker sync.UpdateChecker, dispatcher sync.Dispatcher, messages chan *message.Message) {
	for i := 0; i < workers; i++ {
		go infoWorker(projects, checker, dispatcher, messages)
	}
}

// infoWorker reads a project from the projects channel, runs it through the update check
// and (conditionally) sends it to the dispatcher which loads up the job queue.
func infoWorker(projects <-chan wporg.RepoProject, checker sync.UpdateChecker, dispatcher sync.Dispatcher, messages chan *message.Message) {
	for {
		select {
		case project := <-projects:
			// If this project is not in sync (or forced), then send it to the queue.
			if checker.UpdateCheck(project) || forcedSync {
				if err := dispatcher.Dispatch(project); err == nil {
					// If its been successfully added to the queue then change the
					// project state.
					checker.RecordUpdate(project)
				} else {
					log.Println(project.Name, err)
				}
			}
		}
	}
}

// defaultStandards gets the standards to run against a project.
func defaultStandards() []string {
	return []string{
		"phpcs_wordpress",
		"phpcs_phpcompatibility",
	}
}

// defaultAudits gets the audits to run against a project.
func defaultAudits() *[]message.Audit {
	return &[]message.Audit{
		{
			Type: "phpcs",
			Options: &message.AuditOption{
				Standard: "wordpress",
				Report:   "json",
				Encoding: "utf-8",
				Ignore:   "*/vendor/*,*/node_modules/*",
			},
		},
		{
			Type: "phpcs",
			Options: &message.AuditOption{
				Standard:   "phpcompatibility",
				Report:     "json",
				Encoding:   "utf-8",
				RuntimeSet: "testVersion 5.2-",
			},
		},
	}
}

func main() {

	// SYNC_ACTIVE must be set to "on". This is to ensure you know what you are
	// doing and not flooding message queues.
	if ! serverActive {
		log.Println("Sync Server not active. Please set ENV variable `SYNC_ACTIVE=on`.")
		return;
	}

	// Message channel.
	messages := make(chan *message.Message, perPage)

	token := make(chan struct{}, 1)

	// themeTasks is a channel populated by a fetcher.
	themeTasks := fetcher("themes", browseCategory, perPage, token, -1)

	// Init the dispatcher.
	dispatcher.Init()

	// Start the theme worker pool.
	go pool(poolSize, themeTasks, checker, dispatcher, messages)

	// Send a token.
	token <- struct{}{}

	// Wait before starting the plugin fetcher, so that the theme can
	// pick up the token first.
	time.Sleep(time.Second * 2)

	// pluginTasks is a channel populated by a fetcher.
	pluginTasks := fetcher("plugins", browseCategory, perPage, token, -1)

	// Start the plugin worker pool.
	go pool(poolSize, pluginTasks, checker, dispatcher, messages)

	for {
		select {
		case <-quit:
			return
		}
	}
}
