// SPDX-FileCopyrightText: 2021 Kaelan Thijs Fouwels <kaelan.thijs@fouwels.com>
//
// SPDX-License-Identifier: MIT

package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"tel/config"
	"time"

	pahmqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
)

type MQTT struct {
	device config.MQTTDevice
	tagmap []mqttMap
	opc    *opcua.Client
	mqc    pahmqtt.Client
	buffer map[string]string
}

type mqttMap struct {
	Mqtt   config.MQTTTag
	Tag    config.TagListTag
	NodeID ua.NodeID
}

type mqttMessage struct {
	Timestamp time.Time
	Tag       string
	Value     interface{}
}

func NewMQTT(tags []config.TagListTag, cfg config.MQTTDriver, opc string) (*MQTT, error) {

	mb := MQTT{
		device: cfg.Device,
		buffer: map[string]string{},
	}

	err := mb.tagLoad(tags, cfg.Tags)
	if err != nil {
		return nil, fmt.Errorf("failed to load tags: %w", err)
	}

	mqconfig := pahmqtt.NewClientOptions()
	mqconfig.AddBroker(cfg.Device.Target)
	mqconfig.SetClientID(cfg.Device.ClientID)
	mqconfig.SetUsername(cfg.Device.Username)
	mqconfig.SetPassword(cfg.Device.Token)
	mqconfig.SetKeepAlive(time.Duration(cfg.Device.KeepaliveMs) * time.Millisecond)

	mqc := pahmqtt.NewClient(mqconfig)

	mb.mqc = mqc
	mb.opc = opcua.NewClient(opc)
	return &mb, nil
}

func (m *MQTT) Run(ctx context.Context) error {

	err := m.opc.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect OPC: %w", err)
	}

	token := m.mqc.Connect()
	if token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to connect: %w", token.Error())
	}

	ticker := time.NewTicker(10 * time.Millisecond)

	publisher := time.NewTicker(100 * time.Millisecond)

	for range ticker.C {
		select {
		case <-ctx.Done():
			return fmt.Errorf("ctx caught")
		case <-publisher.C:
			err := m.iotick()
			if err != nil {
				return fmt.Errorf("tick error: %w", err)
			}
		}
	}
	return fmt.Errorf("unexpected exit")
}
func (m *MQTT) tagLoad(tags []config.TagListTag, mtags []config.MQTTTag) error {

	for _, v := range mtags {

		tag := config.TagListTag{}

		for _, x := range tags {
			if v.Name == x.Name {
				tag = x
			}
		}

		if tag.Name == "" {
			return fmt.Errorf("mqtt tag %v was not found in global tag list", v)
		}

		nodeid, err := ua.ParseNodeID("ns=1;s=" + v.Name)
		if err != nil {
			return fmt.Errorf("node id could not be parsed for tag: %+v: %w", v, err)
		}

		record := mqttMap{
			Mqtt:   v,
			Tag:    tag,
			NodeID: *nodeid,
		}

		m.tagmap = append(m.tagmap, record)
	}

	return nil
}

func (m *MQTT) iotick() error {

	for _, v := range m.tagmap {

		req := &ua.ReadRequest{
			MaxAge:             0,
			NodesToRead:        []*ua.ReadValueID{{NodeID: &v.NodeID}},
			TimestampsToReturn: ua.TimestampsToReturnBoth,
		}

		resp, err := m.opc.Read(req)
		if err != nil {
			return fmt.Errorf("failed to read %v (%v): %w", v.Tag.Name, v.NodeID.String(), err)
		}
		if len(resp.Results) < 1 {
			return fmt.Errorf("no results returned for %v (%v)", v.Tag.Name, v.NodeID.String())
		}
		if resp.Results[0].Status != ua.StatusOK {
			return fmt.Errorf("read failed for for %v (%v): %v", v.Tag.Name, v.NodeID.String(), resp.Results[0].Status)
		}

		variant := resp.Results[0].Value
		tp := variant.Type()
		var value interface{}

		switch tp {
		case ua.TypeIDBoolean:
			value = variant.Bool()
		case ua.TypeIDInt16, ua.TypeIDInt32, ua.TypeIDInt64:
			value = variant.Int()
		case ua.TypeIDUint16, ua.TypeIDUint32, ua.TypeIDUint64:
			value = variant.Uint()
		case ua.TypeIDFloat, ua.TypeIDDouble:
			value = variant.Float()
		default:
			return fmt.Errorf("unknown type for tag %v: %v", v.NodeID, variant)
		}

		strval := fmt.Sprintf("%v", value)
		id := v.NodeID.String()
		_, ok := m.buffer[id]
		if !ok {
			m.buffer[id] = strval
		}

		// if no change, skip
		if ok && m.buffer[id] == strval {
			continue
		}

		m.buffer[id] = strval

		p := mqttMessage{
			Timestamp: time.Now(),
			Tag:       v.Tag.Name,
			Value:     value,
		}

		j, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("failed to marshal: %v", err)
		}
		js := string(j)

		log.Printf("publishing to %v: %v", v.Mqtt.Topic, js)

		token := m.mqc.Publish(v.Mqtt.Topic, 0x00, true, js)

		tout := token.WaitTimeout(time.Second * 1)
		if !tout {
			return fmt.Errorf("timed out")
		}
		err = token.Error()
		if err != nil {
			return fmt.Errorf("failed to publish: %w", err)
		}
	}

	return nil
}
