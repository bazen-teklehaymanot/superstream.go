package memphis_kafka

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	clientReconnectionUpdateSubject = "memphis.clientReconnectionUpdate"
	clientTypeUpdateSubject         = "memphis.clientTypeUpdate"
	clientRegisterSubject           = "memphis.registerClient"
	memphisLearningSubject          = "memphis.schema.learnSchema.%v"
	memphisRegisterSchemaSubject    = "memphis.tasks.schema.registerSchema.%v"
	memphisClientUpdatesSubject     = "memphis.updates.%v"
	memphisGetSchemaSubject         = "memphis.schema.getSchema.%v"
	memphisErrorSubject             = "memphis.clientErrors"
)

type Option func(*Options) error

type Options struct {
	Host string
}

type RegisterResp struct {
	ClientID       int    `json:"clientId"`
	AccountName    string `json:"accountName"`
	LearningFactor int    `json:"learningFactor"`
}

type RegisterReq struct {
	NatsConnectionID string `json:"natsConnectiontId"`
	Language         string `json:"language"`
	Version          string `json:"version"`
}

type ClientReconnectionUpdateReq struct {
	NewNatsConnectionID string `json:"newNatsConnectiontId"`
	ClientID            int    `json:"clientId"`
}

type ClientTypeUpdateReq struct {
	ClientID int    `json:"clientId"`
	Type     string `json:"type"`
}

type ClientUpdateSub struct {
	ClientID     int
	Subscription *nats.Subscription
	UpdateCahn   chan Update
}

type Update struct {
	Type    string
	Payload []byte
}

type SchemaUpdateReq struct {
	MsgName  string
	SchemaID int
	Desc     []byte
}

type Client struct {
	ClientID              int
	AccountName           string
	NatsConnectionID      string
	IsConsumer            bool
	IsProducer            bool
	LearningFactor        int
	LearningFactorCounter int
	LearningRequestSent   bool
	GetSchemaRequestSent  bool
	BrokerConnection      *nats.Conn
	JSContext             nats.JetStreamContext
	ProducerProtoDesc     protoreflect.MessageDescriptor
	ProducerSchemaID      int
	ConsumerProtoDescMap  map[int]protoreflect.MessageDescriptor
	ErrorsMsgChan         chan string
}

var ClientConnection *Client

func Init(token string, config interface{}, options ...Option) {
	opts := GetDefaultOptions()
	for _, opt := range options {
		if opt != nil {
			if err := opt(&opts); err != nil {
				fmt.Println("memphis: error initializing memphis: Wrong option")
				return
			}
		}
	}

	ClientConnection = &Client{}

	err := ClientConnection.InitializeNatsConnection(token, opts.Host)
	if err != nil {
		fmt.Println("memphis: error initializing memphis")
		return
	}

	err = ClientConnection.RegisterClient()
	if err != nil {
		fmt.Println("memphis: error registering client")
		return
	}

	err = ClientConnection.SubscribeUpdates()
	if err != nil {
		fmt.Println("memphis: error subscribing to updates")
		return
	}

	startInterceptors(config)
	return
}

func Close() {
	ClientConnection.BrokerConnection.Close()
}

func Host(host string) Option {
	return func(o *Options) error {
		o.Host = host
		return nil
	}
}

func GetDefaultOptions() Options {
	return Options{
		Host: "broker.cost.memphis.dev",
	}
}

func (c *Client) InitializeNatsConnection(token, host string) error {

	splitedToken := strings.Split(token, ":::")
	if len(splitedToken) != 2 {
		return fmt.Errorf("memphis: token is not valid")
	}

	JWT := splitedToken[0]
	Nkey := splitedToken[1]

	opts := []nats.Option{
		nats.UserJWT(
			func() (string, error) { // Callback to return the user JWT
				return JWT, nil
			},
			func(nonce []byte) ([]byte, error) { // Callback to sign the nonce with user's NKey seed
				userNKey, err := nkeys.FromSeed([]byte(Nkey))
				if err != nil {
					return nil, err
				}
				defer userNKey.Wipe()
				return userNKey.Sign(nonce)
			},
		),
		nats.ReconnectHandler(
			func(nc *nats.Conn) {
				natsConnectionID, err := c.generateNatsConnectionID()
				if err != nil {
					handleError(fmt.Sprintf("[sdk: go][version: %v]InitializeNatsConnection at generateNatsConnectionID: %v", sdkVersion, err.Error()))
				}

				clientReconnectionUpdateReq := ClientReconnectionUpdateReq{
					NewNatsConnectionID: natsConnectionID,
					ClientID:            c.ClientID,
				}

				clientReconnectionUpdateReqBytes, err := json.Marshal(clientReconnectionUpdateReq)
				if err != nil {
					handleError(fmt.Sprintf("[sdk: go][version: %v]InitializeNatsConnection at Marshal %v", sdkVersion, err.Error()))
				}

				_, err = nc.Request(clientReconnectionUpdateSubject, clientReconnectionUpdateReqBytes, 30*time.Second)
				if err != nil {
					handleError(fmt.Sprintf("[sdk: go][version: %v]InitializeNatsConnection at nc.Request %v", sdkVersion, err.Error()))
				}

				c.NatsConnectionID = natsConnectionID
			},
		),
	}

	nc, err := nats.Connect(host, opts...)
	if err != nil {
		if strings.Contains(err.Error(), "nats: maximum account") {
			return fmt.Errorf("memphis: can no connect with memphis since you have reached the maximum amount of connected clients")
		} else if strings.Contains(err.Error(), "timeout") {
			return fmt.Errorf("memphis: error connecting to memphis: timeout")
		} else if strings.Contains(err.Error(), "unauthorized") {
			return fmt.Errorf("memphis: error connecting to memphis: unauthorized")
		} else {
			return fmt.Errorf("memphis: error connecting to memphis: %v", err)
		}
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("memphis: error connecting to memphis: %v", err)
	}

	c.BrokerConnection = nc
	c.JSContext = js

	natsConnectionID, err := c.generateNatsConnectionID()
	if err != nil {
		return fmt.Errorf("memphis: error connecting to memphis")
	}
	c.NatsConnectionID = natsConnectionID

	return nil
}

func (c *Client) RegisterClient() error {
	registerReq := RegisterReq{
		NatsConnectionID: c.NatsConnectionID,
		Language:         "go",
		Version:          sdkVersion,
	}

	registerReqBytes, err := json.Marshal(registerReq)
	if err != nil {
		return fmt.Errorf("memphis: error registering client: %v", err)
	}

	resp, err := c.BrokerConnection.Request(clientRegisterSubject, registerReqBytes, 30*time.Second)
	if err != nil {
		return fmt.Errorf("memphis: error registering client: %v", err)
	}

	var registerResp RegisterResp
	err = json.Unmarshal(resp.Data, &registerResp)
	if err != nil {
		return fmt.Errorf("memphis: error registering client: %v", err)
	}

	c.ClientID = registerResp.ClientID
	c.AccountName = registerResp.AccountName
	c.LearningFactor = registerResp.LearningFactor
	c.LearningFactorCounter = 0
	c.LearningRequestSent = false
	c.ConsumerProtoDescMap = make(map[int]protoreflect.MessageDescriptor)
	c.IsConsumer = false
	c.IsProducer = false

	return nil
}

func (c *Client) SubscribeUpdates() error {
	cus := ClientUpdateSub{
		ClientID:   c.ClientID,
		UpdateCahn: make(chan Update),
	}

	go cus.UpdatesHandler()

	var err error
	cus.Subscription, err = c.BrokerConnection.Subscribe(fmt.Sprintf(memphisClientUpdatesSubject, c.ClientID), cus.SubscriptionHandler())
	if err != nil {
		return fmt.Errorf("memphis: error connecting to memphis")
	}

	return nil
}

func (c *ClientUpdateSub) UpdatesHandler() {
	for {
		msg := <-c.UpdateCahn
		switch msg.Type {
		case "LearnedSchema":
			var schemaUpdateReq SchemaUpdateReq
			err := json.Unmarshal(msg.Payload, &schemaUpdateReq)
			if err != nil {
				handleError(fmt.Sprintf("[sdk: go][version: %v]UpdatesHandler at json.Unmarshal: %v", sdkVersion, err.Error()))
			}
			desc := compileMsgDescriptor(schemaUpdateReq.Desc, schemaUpdateReq.MsgName)
			if desc != nil {
				ClientConnection.ProducerProtoDesc = desc
				ClientConnection.ProducerSchemaID = schemaUpdateReq.SchemaID
			} else {
				handleError(fmt.Sprintf("[sdk: go][version: %v]UpdatesHandler: error compiling schema ", sdkVersion))
			}
		}
	}
}

func (c *ClientUpdateSub) SubscriptionHandler() nats.MsgHandler {
	return func(msg *nats.Msg) {
		var update Update
		err := json.Unmarshal(msg.Data, &update)
		if err != nil {
			handleError(fmt.Sprintf("[sdk: go][version: %v]SubscriptionHandler at json.Unmarshal: %v", sdkVersion, err.Error()))
		}
		c.UpdateCahn <- update
	}
}

func SendLearningMessage(msg []byte) {
	_, err := ClientConnection.JSContext.Publish(fmt.Sprintf(memphisLearningSubject, ClientConnection.ClientID), msg)
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]SendLearningMessage at Publish %v", sdkVersion, err.Error()))
	}
}

func SendRegisterSchemaReq() {
	if ClientConnection.LearningRequestSent {
		return
	}
	_, err := ClientConnection.JSContext.Publish(fmt.Sprintf(memphisRegisterSchemaSubject, ClientConnection.ClientID), []byte(""))
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]SendRegisterSchemaReq at Publish %v", sdkVersion, err.Error()))
	} else {
		ClientConnection.LearningRequestSent = true
	}
}

func compileMsgDescriptor(desc []byte, MsgName string) protoreflect.MessageDescriptor {
	descriptorSet := descriptorpb.FileDescriptorSet{}
	err := proto.Unmarshal(desc, &descriptorSet)
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]compileMsgDescriptor at proto.Unmarshal %v", sdkVersion, err.Error()))
		return nil
	}

	localRegistry, err := protodesc.NewFiles(&descriptorSet)
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]compileMsgDescriptor at protodesc.NewFiles %v", sdkVersion, err.Error()))
		return nil
	}

	filePath := fmt.Sprintf("%v.proto", "testDescriptor")
	fileDesc, err := localRegistry.FindFileByPath(filePath)
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]compileMsgDescriptor at FindFileByPath %v", sdkVersion, err.Error()))
		return nil
	}

	msgsDesc := fileDesc.Messages()
	return msgsDesc.ByName(protoreflect.Name(MsgName))
}

func SentGetSchemaRequest(schemaID string) error {
	if ClientConnection.GetSchemaRequestSent {
		return nil
	} else {
		ClientConnection.GetSchemaRequestSent = true
		msg, err := ClientConnection.BrokerConnection.Request(fmt.Sprintf(memphisGetSchemaSubject, ClientConnection.ClientID), []byte(schemaID), 30*time.Second)
		if err != nil {
			handleError(fmt.Sprintf("[sdk: go][version: %v]compileMsgDescriptor at Request %v", sdkVersion, err.Error()))
			ClientConnection.GetSchemaRequestSent = false
			return err
		}
		var resp SchemaUpdateReq
		err = json.Unmarshal(msg.Data, &resp)
		if err != nil {
			handleError(fmt.Sprintf("[sdk: go][version: %v]compileMsgDescriptor at json.Unmarshal %v", sdkVersion, err.Error()))
			ClientConnection.GetSchemaRequestSent = false
			return err
		}
		desc := compileMsgDescriptor(resp.Desc, resp.MsgName)
		if desc != nil {
			ClientConnection.ConsumerProtoDescMap[resp.SchemaID] = desc
		} else {
			handleError(fmt.Sprintf("[sdk: go][version: %v]compileMsgDescriptor: error compiling schema", sdkVersion))
			ClientConnection.GetSchemaRequestSent = false
			return fmt.Errorf("memphis: error compiling schema")
		}
		return nil
	}
}

func SendClientTypeUpdateReq(clientID int, clientType string) {
	clientTypeUpdateReq := ClientTypeUpdateReq{
		ClientID: clientID,
		Type:     clientType,
	}

	clientTypeUpdateReqBytes, err := json.Marshal(clientTypeUpdateReq)
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]SendClientTypeUpdateReq at json.Marshal %v", sdkVersion, err.Error()))
	}

	_, err = ClientConnection.JSContext.Publish(clientTypeUpdateSubject, clientTypeUpdateReqBytes)
	if err != nil {
		handleError(fmt.Sprintf("[sdk: go][version: %v]SendClientTypeUpdateReq at Publish %v", sdkVersion, err.Error()))
	}
	switch clientType {
	case "consumer":
		ClientConnection.IsConsumer = true
	case "producer":
		ClientConnection.IsProducer = true
	}
}

func (c *Client) generateNatsConnectionID() (string, error) {
	natsConnectionId, err := c.BrokerConnection.GetClientID()
	if err != nil {
		return "", err
	}

	serverName := c.BrokerConnection.ConnectedServerName()

	return fmt.Sprintf("%v:%v", serverName, natsConnectionId), nil
}

func sendClientErrorsToBE(errMsg string) {
	ClientConnection.BrokerConnection.Publish(memphisErrorSubject, []byte(errMsg))
}
