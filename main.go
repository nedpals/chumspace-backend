package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/labstack/echo/v5"
	lkAuth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/models"
	"golang.org/x/exp/slices"
	"google.golang.org/api/option"
)

func initializeFirebase() *firebase.App {
	isJson := false

	credsPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if len(credsPath) == 0 {
		panic("GOOGLE_APPLICATION_CREDENTIALS is not set")
	}

	contents, err := os.ReadFile(credsPath)
	if err != nil {
		osErr := err

		// try to read the file as a json string
		var dummy map[string]interface{}
		if err := json.Unmarshal([]byte(credsPath), &dummy); err != nil {
			panic(fmt.Sprintf("error reading credentials file: %v\n", osErr))
		} else {
			contents = []byte(credsPath)
			isJson = true
		}
	}

	opts := []option.ClientOption{}

	if isJson {
		opts = append(opts, option.WithCredentialsJSON(contents))
	} else {
		opts = append(opts, option.WithCredentialsFile(credsPath))
	}

	app, err := firebase.NewApp(context.Background(), nil, opts...)
	if err != nil {
		panic(fmt.Sprintf("error initializing app: %v\n", err))
	}
	return app
}

func makeChatIdentifier(fromChatType string, chatId string) string {
	return fromChatType + ":" + chatId
}

func main() {
	app := pocketbase.New()

	// serves static files from the provided public dir (if exists)
	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		e.Router.Use(apis.ActivityLogger(e.App))

		// notification scheduler
		notifScheduler, monitorNotifications := startSchedulingNotifications()

		// firebase
		firebaseApp := initializeFirebase()
		messagingClient, err := firebaseApp.Messaging(context.Background())
		if err != nil {
			return err
		}

		// set the messaging client of the notification scheduler
		notifScheduler.MessagingClient = messagingClient

		// get the livekit host
		lkHost, lkHostExists := os.LookupEnv("LIVEKIT_SERVER_URL")
		if !lkHostExists {
			return fmt.Errorf("LIVEKIT_SERVER_URL is not set")
		}

		// get the livekit api key and secret first
		lkApiKey, lkApiKeyExists := os.LookupEnv("LIVEKIT_API_KEY")
		if !lkApiKeyExists {
			return fmt.Errorf("LIVEKIT_API_KEY is not set")
		}

		lkApiSecret, lkApiSecretExists := os.LookupEnv("LIVEKIT_API_SECRET")
		if !lkApiSecretExists {
			return fmt.Errorf("LIVEKIT_API_SECRET is not set")
		}

		lkRoomClient := lksdk.NewRoomServiceClient(lkHost, lkApiKey, lkApiSecret)

		e.Router.Add("POST", "/api/test_fcm", func(c echo.Context) error {
			// get the token from query params
			token := c.QueryParam("token")
			if len(token) == 0 {
				return apis.NewBadRequestError("token is required", nil)
			}

			// reverse for prod to avoid abuse
			isDev := c.QueryParam("key") == "jeff2"

			// construct the message
			ttl := time.Duration(10) * time.Second
			notifId, _ := gonanoid.New()
			var notif *messaging.Notification

			if isDev {
				notif = &messaging.Notification{
					Title: "Test FCM",
					Body:  "This is a test notification",
				}
			}

			err := sendNotification(notifId, &messaging.Message{
				Data: map[string]string{
					"type": "test_fcm",
				},
				Notification: notif,
				Android: &messaging.AndroidConfig{
					TTL: &ttl,
				},
				Token: token,
			}, nil, notifScheduler.MessagingClient)

			if err != nil {
				return apis.NewBadRequestError(err.Error(), nil)
			}

			return c.JSON(http.StatusOK, map[string]string{
				"message": "success",
			})
		}, apis.RequireRecordAuth())

		e.Router.Add("POST", "/api/join_call", func(c echo.Context) error {
			// get the room type
			fromChatType := c.QueryParam("from_chat_type") // ds or parent
			if len(fromChatType) == 0 {
				// from_room_type for legacy
				fromChatType = c.QueryParam("from_room_type")
			}

			if fromChatType != "ds" && fromChatType != "parent" {
				return apis.NewBadRequestError("invalid room type", nil)
			}

			// get the call type
			callType := c.QueryParamDefault("type", "audio")

			// get the chat info
			chatId := c.QueryParam("chat_id")
			if len(chatId) == 0 {
				return apis.NewBadRequestError("chat_id is required", nil)
			}

			chat, err := app.Dao().FindRecordById("chat_list_"+fromChatType, chatId)
			if err != nil {
				return err
			}

			apis.EnrichRecord(c, app.Dao(), chat, "chatRequestTo", "chatRequestBy")

			// get the user
			user := apis.RequestInfo(c).AuthRecord

			// identity != participantName, only used for JWT
			identity := user.Id
			participantName := user.GetString("name")

			// create a room or get the room if it already exists
			roomCollection, err := app.Dao().FindCollectionByNameOrId("call_rooms")
			if err != nil {
				return err
			}

			// get first room that user is already in
			existingJoinedRoom, _ := app.Dao().FindFirstRecordByFilter(roomCollection.Id, "participants~{:user}", dbx.Params{"user": user.Id})

			// throw an error that user has already joined (if they have)
			//
			// if they have already joined, chances are:
			// - they are trying to join a call from a different chat
			// - they are trying to join a call from the same chat
			// - they are trying to join a call from the same chat, but they are already in a call from a different devcie
			if existingJoinedRoom != nil {
				return apis.NewForbiddenError("You have already joined a call", nil)
			}

			// get the room if it already exists
			roomRecord, err := app.Dao().FindFirstRecordByFilter(roomCollection.Id, "from_chat={:from_chat}", dbx.Params{
				"from_chat": makeChatIdentifier(fromChatType, chat.Id),
			})
			if err != nil {
				// create the room if it doesn't exist
				roomRecord = models.NewRecord(roomCollection)
				roomRecord.Set("from_chat", makeChatIdentifier(fromChatType, chat.Id))
				roomRecord.Set("hosts", []string{user.Id})
				roomRecord.Set("participants", []string{})
				roomRecord.Set("invited_participants", []string{
					chat.ExpandedOne("chatRequestTo").GetString("users"),
					chat.ExpandedOne("chatRequestBy").GetString("users"),
				})
			}

			isRoomExisting := len(roomRecord.Id) != 0 && !roomRecord.IsNew()

			// add the user to the room
			if !slices.Contains(roomRecord.GetStringSlice("invited_participants"), user.Id) {
				return apis.NewForbiddenError("forbidden to join this room", nil)
			}

			participants := roomRecord.GetStringSlice("participants")
			if !slices.Contains(participants, user.Id) {
				participants = append(participants, user.Id)
				roomRecord.Set("participants", participants)
				app.Dao().SaveRecord(roomRecord)
			}

			// list of grants and other info to be permitted to the user
			isHost := slices.Contains(roomRecord.GetStringSlice("hosts"), user.Id)
			isParticipant := true

			at := lkRoomClient.CreateToken()
			grant := &lkAuth.VideoGrant{
				Room:         roomRecord.Id,
				RoomJoin:     true,
				CanPublish:   &isParticipant,
				CanSubscribe: &isParticipant,
				RoomAdmin:    isHost,
			}

			at.AddGrant(grant).
				SetIdentity(identity).
				SetName(participantName).
				SetMetadata(user.Id).
				SetValidFor(6 * 60 * 60)

			// create a JWT token
			token, err := at.ToJWT()
			if err != nil {
				return err
			}

			// notify other invited participants
			if !isRoomExisting {
				inviteeJson, _ := json.Marshal(user.PublicExport())
				tokens := []string{}

				err := apis.EnrichRecord(c, app.Dao(), roomRecord, "invited_participants")
				if err == nil {
					participants := roomRecord.GetStringSlice("participants")
					invitedParticipants := roomRecord.ExpandedAll("invited_participants")

					// get the tokens of the invited participants
					for _, participant := range invitedParticipants {
						if participant.Id == user.Id || slices.Contains(participants, participant.Id) {
							continue
						}

						participantTokens := participant.GetStringSlice("fcm_tokens")
						tokens = append(tokens, participantTokens...)
					}
				}

				if len(tokens) != 0 {
					// construct the message
					ttl := time.Duration(5) * time.Minute
					imageUrl := "" // picture of user with no avatar
					if avatar := user.GetString("avatar"); len(avatar) != 0 {
						gotImageUrl, err := url.JoinPath(app.Settings().Meta.AppUrl, "api/files/users", user.Id, avatar)
						if err == nil {
							imageUrl = gotImageUrl
						}
					}

					notifScheduler.AddNotification(&ScheduledNotification{
						MulticastMessage: &messaging.MulticastMessage{
							Data: map[string]string{
								"type":           "incoming_call",
								"call_type":      callType,
								"invitee":        string(inviteeJson),
								"chat_id":        chat.Id,
								"from_chat_type": fromChatType,
								"image_url":      imageUrl,
							},
							Notification: &messaging.Notification{
								Title:    "Incoming Call",
								Body:     participantName + " is inviting you to a call",
								ImageURL: imageUrl,
							},
							Android: &messaging.AndroidConfig{
								Priority: "high",
								TTL:      &ttl,
							},
							Tokens: tokens,
						},
						ScheduledTime: time.Now().Add(2 * time.Second),
					})
				}
			}

			// return the token
			return c.JSON(http.StatusOK, map[string]string{
				"token": token,
				"room":  roomRecord.Id,
			})
		}, apis.RequireRecordAuth())

		// this route is for the invited participants to respond the call
		e.Router.Add("POST", "/api/room_data", func(c echo.Context) error {
			fromChatType := c.QueryParam("from_chat_type") // ds or parent
			if len(fromChatType) == 0 {
				// from_room_type for legacy
				fromChatType = c.QueryParam("from_room_type")
			}

			if fromChatType != "ds" && fromChatType != "parent" {
				return apis.NewBadRequestError("invalid room type", nil)
			}

			chatId := c.QueryParam("chat_id")
			if len(chatId) == 0 {
				return apis.NewBadRequestError("chat_id is required", nil)
			}

			user := apis.RequestInfo(c).AuthRecord
			room, err := app.Dao().FindFirstRecordByFilter("call_rooms", "from_chat={:from_chat} && invited_participants~{:user}",
				dbx.Params{
					"from_chat": makeChatIdentifier(fromChatType, chatId),
					"user":      user.Id,
				})
			if err != nil {
				return apis.NewNotFoundError("room not found", nil)
			}

			rawPayloadData := map[string]any{}
			if status := c.QueryParam("status"); len(status) != 0 {
				switch status {
				case "rejected", "accepted", "declined":
					if status == "rejected" {
						status = "declined"
					}

					if status == "declined" && len(room.GetStringSlice("participants")) == 1 {
						rawPayloadData["disconnect"] = true
						rawPayloadData["disconnect_reason"] = fmt.Sprintf("Call %s by %s", status, user.GetString("name"))
					}

					rawPayloadData["call_status"] = status
					rawPayloadData["call_status_by"] = user.Id
				}
			}

			if len(rawPayloadData) == 0 {
				return apis.NewBadRequestError("no data to send", nil)
			}

			payloadData, err := json.Marshal(rawPayloadData)
			if err != nil {
				return err
			}

			_, err = lkRoomClient.SendData(c.Request().Context(), &livekit.SendDataRequest{
				Room: room.Id,
				Kind: livekit.DataPacket_RELIABLE,
				Data: payloadData,
			})

			if err != nil {
				return err
			}

			return c.JSON(http.StatusOK, map[string]string{
				"message": "ok",
			})
		}, apis.RequireRecordAuth())

		e.Router.Add("POST", "/api/leave_call", func(c echo.Context) error {
			// get the chat info
			fromError := c.QueryParam("from_error") == "1"
			fromChatType := c.QueryParam("from_chat_type") // ds or parent
			if len(fromChatType) == 0 {
				// from_room_type for legacy
				fromChatType = c.QueryParam("from_room_type")
			}

			if fromChatType != "ds" && fromChatType != "parent" {
				return apis.NewBadRequestError("from_chat_type is required", nil)
			}

			chatId := c.QueryParam("chat_id")
			if len(chatId) == 0 {
				return apis.NewBadRequestError("chat_id is required", nil)
			}

			user := apis.RequestInfo(c).AuthRecord
			room, err := app.Dao().FindFirstRecordByFilter("call_rooms", "from_chat={:from_chat} && participants~{:user}", dbx.Params{
				"from_chat": makeChatIdentifier(fromChatType, chatId),
				"user":      user.Id,
			})
			if err == nil {
				participants := room.GetStringSlice("participants")
				// if the user is the last participant, remove the room
				if len(participants)-1 <= 0 {
					app.Dao().DeleteRecord(room)
				} else {
					participantIdx := slices.Index(participants, user.Id)
					participants = slices.Delete(participants, participantIdx, participantIdx+1)
					room.Set("participants", participants)
					app.Dao().SaveRecord(room)
				}
			} else if !fromError {
				return apis.NewNotFoundError("room not found", nil)
			}

			// passively return success if fromError or if the operation was done successfully
			return c.JSON(http.StatusOK, map[string]bool{
				"message": true,
			})
		})

		// launch the notification scheduler
		go monitorNotifications()

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatalln(err)
	}
}
