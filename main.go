package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ahaynssen/slack-go-webhook"
	"github.com/objectpartners/rancher-service-updater/utils"
	"github.com/rancher/go-rancher/client"
)

type (
	//Config service configuration
	Config struct {
		EnableLabel      string
		EnvironmentNames []string
		Port             int
		CattleSecretKey  string
		CattleAccessKey  string
		CattleURL        string
		SlackWebhookURL  string
		SlackBotName     string
	}

	//ServiceUpdater the service
	ServiceUpdater struct {
		Config *Config
		client *client.RancherClient
	}

	//UpdateCommand payload for new image availability
	UpdateCommand struct {
		Image      string `json:"docker_image"`
		StartFirst bool   `json:"start_first"`
		Confirm    bool   `json:"confirm"`
		Timeout    int    `json:"timeout"`
	}
)

func main() {
	config := &Config{
		EnableLabel:      utils.GetEnvOrDefault("AUTOUPDATE_ENABLE_LABEL", "autoupdate.enable"),
		EnvironmentNames: utils.GetEnvOrDefaultArray("AUTOUPDATE_ENVIRONMENT_NAMES", []string{".*"}),
		Port:             utils.GetEnvOrDefaultInt("AUTOUPDATE_HTTP_PORT", 8080),
		CattleAccessKey:  os.Getenv("CATTLE_ACCESS_KEY"),
		CattleSecretKey:  os.Getenv("CATTLE_SECRET_KEY"),
		CattleURL:        os.Getenv("CATTLE_URL"),
		SlackWebhookURL:  os.Getenv("AUTOUPDATE_SLACK_WEBHOOK_URL"),
		SlackBotName:     utils.GetEnvOrDefault("AUTOUPDATE_SLACK_BOT_NAME", "rancher-service-updater"),
	}
	serviceUpdater := &ServiceUpdater{
		Config: config,
	}
	serviceUpdater.init()
	serviceUpdater.listen()
}

func (s *ServiceUpdater) init() {
	c, err := client.NewRancherClient(&client.ClientOpts{
		AccessKey: s.Config.CattleAccessKey,
		SecretKey: s.Config.CattleSecretKey,
		Url:       s.Config.CattleURL,
	})
	if err != nil {
		log.Fatalf("Unable to create Rancher client: %s\n", err)
	}
	s.client = c
}

func (s *ServiceUpdater) listen() {
	http.HandleFunc("/upgrade", s.upgrade)
	http.HandleFunc("/ping", s.ping)
	err := http.ListenAndServe(fmt.Sprintf(":%d", s.Config.Port), nil)
	if err != nil {
		log.Fatalf("Unable to start service on port %d\n", s.Config.Port)
	} else {
		log.Printf("Started service on port %d\n", s.Config.Port)
	}
}

func (s *ServiceUpdater) ping(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Pong!"))
	return
}

func (s *ServiceUpdater) upgrade(w http.ResponseWriter, r *http.Request) {
	command := UpdateCommand{
		StartFirst: false,
		Timeout:    30,
	}

	err := json.NewDecoder(r.Body).Decode(&command)
	if err != nil {
		log.Printf("%s\n", err.Error())
		utils.SendError(w, err.Error(), 400)
		return
	}
	go s.upgradeService(command)
	w.WriteHeader(200)
	return
}

func (s *ServiceUpdater) upgradeService(command UpdateCommand) {
	var wantedImage, wantedVer string
	if !strings.HasPrefix(command.Image, "docker:") {
		command.Image = fmt.Sprintf("docker:%s", command.Image)
	}
	parts := strings.Split(command.Image, ":")
	wantedImage = parts[1]
	wantedVer = parts[2]

	services, err := s.client.Service.List(&client.ListOpts{})
	if err != nil {
		fmt.Printf("Failed to list rancher services: %s\n", err)
		return
	}

	environments, err := s.client.Account.List(&client.ListOpts{})
	if err != nil {
		fmt.Printf("Failed to get environments: %s\n", err)
		return
	}
	envs := make(map[string]string)
	for environments != nil {
		for _, env := range environments.Data {
			envs[env.Id] = env.Name
		}
		environments, err = environments.Next()
		if err != nil {
			fmt.Printf("Failed: %s\n", err)
			return
		}
	}

	var enabledLabel = s.Config.EnableLabel
	for services != nil {
		for _, svc := range services.Data {
			if svc.LaunchConfig != nil {
				if _, ok := svc.LaunchConfig.Labels[enabledLabel]; ok {
					parts := strings.Split(svc.LaunchConfig.ImageUuid, ":")
					foundImage := parts[1]
					foundVer := parts[2]
					if utils.EnvironmentEnabled(envs[svc.AccountId], s.Config.EnvironmentNames) {
						if foundImage == wantedImage && ((foundVer < wantedVer) || (wantedVer == "latest")) {
							fmt.Println("Trying to upgrade...")
							err := s.doUpgrade(command, svc)
							if err != nil {
								fmt.Println(err.Error())
							} else {
								if command.Confirm {
									fmt.Println("Trying to confirm...")
									err := s.confirmUpgrade(command, svc)
									url := fmt.Sprintf("%s/env/%s/apps/stacks/%s", s.Config.CattleURL, svc.AccountId, svc.EnvironmentId)
									if err != nil {
										fmt.Printf("Unable to upgrade service %s: %s\n", svc.Name, err.Error())
										message := fmt.Sprintf("Unable to confirm upgrade to `%s`.\nCheck status at <%[2]s|%[1]s>", svc.Name, url)
										s.slackMessage("danger", message)
									} else {
										fmt.Printf("Upgraded %s to %s\n", svc.Name, command.Image)
										message := fmt.Sprintf("`%[1]s` has been successfully upgraded to `%[2]s` "+
											"in %[4]s\n View in Rancher here: <%[3]s|%[1]s>", svc.Name, wantedVer, url, envs[svc.AccountId])
										s.slackMessage("good", message)

									}
								}
							}
							continue
						}
					}
				}
			}
		}
		services, _ = services.Next()
	}
}

func (s *ServiceUpdater) doUpgrade(command UpdateCommand, service client.Service) error {
	service.LaunchConfig.ImageUuid = command.Image
	upgrade := &client.ServiceUpgrade{}
	upgrade.InServiceStrategy = &client.InServiceUpgradeStrategy{
		LaunchConfig:           service.LaunchConfig,
		SecondaryLaunchConfigs: service.SecondaryLaunchConfigs,
		StartFirst:             command.StartFirst,
	}
	upgrade.ToServiceStrategy = &client.ToServiceUpgradeStrategy{}
	_, err := s.client.Service.ActionUpgrade(&service, upgrade)
	return err
}

func (s *ServiceUpdater) confirmUpgrade(command UpdateCommand, service client.Service) error {
	srv, err := utils.Retry(func() (interface{}, error) {
		s, e := s.client.Service.ById(service.Id)
		if e != nil {
			return nil, e
		}
		if s.State != "upgraded" {
			return nil, fmt.Errorf("Service not upgraded: %s\n", s.State)
		}
		return s, nil
	}, time.Duration(command.Timeout)*time.Second, 3*time.Second)
	if err != nil {
		return err
	}

	srv, err = s.client.Service.ActionFinishupgrade(srv.(*client.Service))
	if err != nil {
		return err
	}
	fmt.Printf("Finished upgrade on %s\n", srv.(*client.Service).Name)
	return err
}

func (s *ServiceUpdater) slackMessage(status string, message string) {
	if s.Config.SlackWebhookURL != "" {
		attachment := slack.Attachment{Color: &status, Text: &message}
		mrkdwn := "text"
		attachment.AddMrkdwn(&mrkdwn)
		payload := slack.Payload{
			Username:    s.Config.SlackBotName,
			Attachments: []slack.Attachment{attachment},
		}
		printable, _ := json.Marshal(payload)
		fmt.Println(string(printable))
		err := slack.Send(s.Config.SlackWebhookURL, "", payload)
		if len(err) > 0 {
			fmt.Printf("error sending Slack message: %s\n", err)
		}
	}
}
