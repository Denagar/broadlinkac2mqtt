package main

import (
	"context"
	"errors"
	"github.com/ArtVladimirov/BroadlinkAC2Mqtt/app"
	"github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/mqtt"
	workspaceMqttModels "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/mqtt/models"
	workspaceMqttSender "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/mqtt/publisher"
	workspaceMqttReceiver "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/mqtt/subscriber"
	workspaceCache "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/repository/cache"
	workspaceService "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/service"
	workspaceServiceModels "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/service/models"
	workspaceWebClient "github.com/ArtVladimirov/BroadlinkAC2Mqtt/app/webClient"
	"github.com/ArtVladimirov/BroadlinkAC2Mqtt/config"
	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type App struct {
	devices             []workspaceServiceModels.DeviceConfig
	topicPrefix         string
	wsBroadLinkReceiver app.WebClient
	wsMqttReceiver      app.MqttSubscriber
	wsService           app.Service
	client              paho.Client
}

func NewApp(logger *zerolog.Logger) (*App, error) {

	// Configuration
	cfg, err := config.NewConfig(logger)
	if err != nil {
		return nil, err
	}

	// Set logger lever
	switch cfg.Service.LogLevel {
	case "error":
		logger.Level(zerolog.ErrorLevel)
	case "debug":
		logger.Level(zerolog.DebugLevel)
	case "fatal":
		logger.Level(zerolog.FatalLevel)
	case "disabled":
		logger.Level(zerolog.Disabled)
	case "info":
		logger.Level(zerolog.InfoLevel)
	default:
		logger.Level(zerolog.ErrorLevel)
	}

	mqttConfig := workspaceMqttModels.ConfigMqtt{
		Broker:                   cfg.Mqtt.Broker,
		User:                     cfg.Mqtt.User,
		Password:                 cfg.Mqtt.Password,
		ClientId:                 cfg.Mqtt.ClientId,
		TopicPrefix:              cfg.Mqtt.TopicPrefix,
		AutoDiscovery:            cfg.Mqtt.AutoDiscovery,
		AutoDiscoveryTopic:       cfg.Mqtt.AutoDiscoveryTopic,
		AutoDiscoveryTopicRetain: cfg.Mqtt.AutoDiscoveryTopicRetain,
	}

	// Repository

	cache := workspaceCache.NewCache()

	opts, _ := mqtt.NewMqttConfig(logger, cfg.Mqtt)
	client := paho.NewClient(opts)

	//Configure MQTT Sender Layer
	mqttSender := workspaceMqttSender.NewMqttSender(
		mqttConfig,
		client,
	)

	//Configure Service Layer
	service := workspaceService.NewService(
		cfg.Mqtt.TopicPrefix,
		cfg.Service.UpdateInterval,
		mqttSender,
		workspaceWebClient.NewWebClient(),
		cache,
	)
	//Configure MQTT Receiver Layer
	mqttReceiver := workspaceMqttReceiver.NewMqttReceiver(
		service,
		mqttConfig,
	)

	var devices []workspaceServiceModels.DeviceConfig
	for _, device := range cfg.Devices {

		if len(device.Mac) != 12 {
			msg := "mac address is wrong"
			logger.Info().Str("mac", device.Mac).Msg(msg)
			return nil, errors.New(msg)
		}

		devices = append(devices, workspaceServiceModels.DeviceConfig{
			Ip:   device.Ip,
			Mac:  strings.ToLower(device.Mac),
			Name: device.Name,
			Port: device.Port,
		})
	}

	application := &App{
		wsMqttReceiver: mqttReceiver,
		client:         client,
		devices:        devices,
		wsService:      service,
		topicPrefix:    cfg.Mqtt.TopicPrefix,
	}

	return application, nil
}

func (app *App) Run(ctx context.Context, logger *zerolog.Logger) error {

	// Run MQTT
	if token := app.client.Connect(); token.Wait() && token.Error() != nil {
		err := token.Error()
		if err != nil {
			logger.Error().Msg("Failed to connect mqtt")
			return err
		}
	}

	// Create Device
	for _, device := range app.devices {
		_, err := app.wsService.CreateDevice(ctx, logger, &workspaceServiceModels.CreateDeviceInput{
			Config: workspaceServiceModels.DeviceConfig{
				Mac:  device.Mac,
				Ip:   device.Ip,
				Name: device.Name,
				Port: device.Port,
			}})
		if err != nil {
			logger.Error().Err(err).Msg("Failed to create the device")
			return err
		}
	}

	for _, device := range app.devices {
		device := device
		go func() {
			for {
				err := app.wsService.AuthDevice(ctx, logger, &workspaceServiceModels.AuthDeviceInput{Mac: device.Mac})
				if err == nil {
					break
				}
				logger.Error().Err(err).Str("device", device.Mac).Msg("Failed to Auth device " + device.Mac + ". Reconnect in 3 seconds...")
				time.Sleep(time.Second * 3)
			}

			// Subscribe on MQTT handlers
			workspaceMqttReceiver.Routers(logger, device.Mac, app.topicPrefix, app.client, app.wsMqttReceiver)

			//Publish Discovery Topic
			err := app.wsService.PublishDiscoveryTopic(ctx, logger, &workspaceServiceModels.PublishDiscoveryTopicInput{Device: device})
			if err != nil {
				return
			}

			err = app.wsService.StartDeviceMonitoring(ctx, logger, &workspaceServiceModels.StartDeviceMonitoringInput{Mac: device.Mac})
			if err != nil {
				return
			}
		}()
	}

	// Graceful shutdown
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGKILL, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	killSignal := <-interrupt
	switch killSignal {
	case syscall.SIGKILL:
		logger.Info().Msg("Got SIGKILL...")
	case syscall.SIGQUIT:
		logger.Info().Msg("Got SIGQUIT...")
	case syscall.SIGTERM:
		logger.Info().Msg("Got SIGTERM...")
	case syscall.SIGINT:
		logger.Info().Msg("Got SIGINT...")
	default:
		logger.Info().Msg("Undefined killSignal...")
	}
	// Publish offline states for devices
	g := new(errgroup.Group)
	for _, device := range app.devices {
		device := device
		g.Go(func() error {
			err := app.wsService.UpdateDeviceAvailability(ctx, logger, &workspaceServiceModels.UpdateDeviceAvailabilityInput{
				Mac:          device.Mac,
				Availability: "offline",
			})
			if err != nil {
				logger.Error().Err(err).Str("device", device.Mac).Msg("Failed to update availability")
				return err
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	// Disconnect MQTT
	app.client.Disconnect(100)

	return nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const skipFrameCount = 0
	logger := zerolog.New(os.Stdout).With().Timestamp().CallerWithSkipFrameCount(zerolog.CallerSkipFrameCount + skipFrameCount).Logger()

	application, err := NewApp(&logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to get a new App")
	}

	// Run
	err = application.Run(ctx, &logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to get a new App")
	}
}
