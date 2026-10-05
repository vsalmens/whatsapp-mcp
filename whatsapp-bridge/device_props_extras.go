package main

// device_props_extras.go — what the bridge tells the phone about itself when it is linked.
//
// whatsmeow's defaults register the device as "whatsmeow" with platform UNKNOWN and without a
// full history sync, so the phone sends only a short recent window at link time. Here the device
// asks for the full history (like WhatsApp Desktop's "full chat history" option), registers as a
// desktop client and announces on-demand history support.
//
// These values are sent only when a device is linked (QR code); they do not change an existing link.
// The name shown in the phone's "Linked devices" list is WHATSAPP_DEVICE_NAME (default below);
// keep it neutral, it is visible to WhatsApp.

import (
	"os"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

const defaultDeviceName = "Home archive"

func init() {
	name := os.Getenv("WHATSAPP_DEVICE_NAME")
	if name == "" {
		name = defaultDeviceName
	}
	store.SetOSInfo(name, [3]uint32{1, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.DeviceProps.RequireFullSync = proto.Bool(true)

	cfg := store.DeviceProps.HistorySyncConfig
	cfg.FullSyncDaysLimit = proto.Uint32(3650)
	cfg.FullSyncSizeMbLimit = proto.Uint32(10240)
	cfg.OnDemandReady = proto.Bool(true)
	cfg.CompleteOnDemandReady = proto.Bool(true)
}
