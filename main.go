package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/labstack/echo/v5"
	lkAuth "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go"
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

func makeChatIdentifierRecord(r *models.Record) string {
	fromChatType := "ds"
	if r.Collection().Name == "chat_list_parent" {
		fromChatType = "parent"
	} else if r.Collection().Name == "chat_list_gc" {
		fromChatType = "community"
	}

	return makeChatIdentifier(fromChatType, r.Id)
}

var validFromChatTypes = []string{"ds", "parent", "community"}

func decodeCallDetailsParams(c echo.Context) (fromChatType string, chatId string, err error) {
	fromChatType = c.QueryParam("from_chat_type") // ds or parent
	if len(fromChatType) == 0 {
		// from_room_type for legacy
		fromChatType = c.QueryParam("from_room_type")
	}

	if fromChatType == "chum" {
		fromChatType = "ds"
	}

	if !slices.Contains(validFromChatTypes, fromChatType) {
		err = apis.NewBadRequestError("invalid room type", nil)
		if len(fromChatType) == 0 {
			err = apis.NewBadRequestError("from_chat_type is required", nil)
		}
		return
	}

	chatId = c.QueryParam("chat_id")
	if len(chatId) == 0 {
		err = apis.NewBadRequestError("chat_id is required", nil)
		return
	}

	return
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
			message := &messaging.Message{
				Data: map[string]string{
					"type": "test_fcm",
				},
				Notification: &messaging.Notification{
					Title: "Test FCM",
					Body:  "This is a test notification",
				},
				Android: &messaging.AndroidConfig{
					TTL: &ttl,
				},
				Token: token,
			}

			if isDev {
				_, err := notifScheduler.MessagingClient.Send(context.Background(), message)
				if err != nil {
					return fmt.Errorf("error sending notification: %v", err)
				}
			} else {
				_, err := notifScheduler.MessagingClient.SendDryRun(context.Background(), message)
				if err != nil {
					return fmt.Errorf("error sending notification: %v", err)
				}
			}

			if err != nil {
				return apis.NewBadRequestError(err.Error(), nil)
			}

			return c.JSON(http.StatusOK, map[string]string{
				"message": "success",
			})
		}, apis.RequireRecordAuth())

		e.Router.Add("POST", "/api/join_call", func(c echo.Context) error {
			// get the room type and chat id
			fromChatType, chatId, err := decodeCallDetailsParams(c)
			if err != nil {
				return err
			}

			// get the call type
			callType := c.QueryParamDefault("type", "audio")

			// get the chat info
			chatListCollectionName := "chat_list_" + fromChatType
			if fromChatType == "community" {
				chatListCollectionName = "chat_list_gc"
			}

			chat, err := app.Dao().FindRecordById(chatListCollectionName, chatId)
			if err != nil {
				return err
			}

			if fromChatType == "community" {
				apis.EnrichRecord(c, app.Dao(), chat, "community", "parents")
			} else {
				apis.EnrichRecord(c, app.Dao(), chat, "chatRequestTo", "chatRequestBy")
			}

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
				hosts := []string{user.Id}

				// create the room if it doesn't exist
				roomRecord = models.NewRecord(roomCollection)

				if fromChatType == "community" {
					expandedCommunity := chat.ExpandedOne("community")

					// include community account in hosts
					hosts = append(hosts, expandedCommunity.GetString("users"))

					expandedParents := chat.ExpandedAll("parents")
					invitedParticipants := make([]string, 1+len(expandedParents)) // community user + parents

					invitedParticipants[0] = expandedCommunity.GetString("users")
					for idx, parent := range expandedParents {
						invitedParticipants[idx+1] = parent.GetString("users")
					}

					roomRecord.Set("invited_participants", invitedParticipants)
				} else {
					roomRecord.Set("invited_participants", []string{
						chat.ExpandedOne("chatRequestTo").GetString("users"),
						chat.ExpandedOne("chatRequestBy").GetString("users"),
					})
				}

				roomRecord.Set("from_chat", makeChatIdentifier(fromChatType, chat.Id))
				roomRecord.Set("hosts", hosts)
				roomRecord.Set("participants", []string{})
			}

			isRoomExisting := len(roomRecord.Id) != 0 && !roomRecord.IsNew()

			// do not allow the user to join if they are not invited
			if !slices.Contains(roomRecord.GetStringSlice("invited_participants"), user.Id) {
				return apis.NewForbiddenError("forbidden to join this room", nil)
			}

			// add the user to the room if they are not already in it
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

				if err := apis.EnrichRecord(c, app.Dao(), roomRecord, "invited_participants"); err == nil {
					participants := roomRecord.GetStringSlice("participants")
					invitedParticipants := roomRecord.ExpandedAll("invited_participants")

					// get the tokens of the invited participants
					for _, participant := range invitedParticipants {
						if participant.Id == user.Id || slices.Contains(participants, participant.Id) {
							continue
						}

						fmt.Printf("[call_room:%s] Notifying %s (%s)\n", roomRecord.Id, participant.GetString("name"), participant.Id)
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

					// separate notification data to be put into data payload
					// as a JSON string to be parsed by the app
					//
					// this is to avoid FCM from automatically showing the notification
					notifJson, _ := json.Marshal(map[string]any{
						"id":         1, // 1 for incoming call
						"type":       "incoming_call",
						"title":      "Incoming Call",
						"body":       participantName + " is inviting you to a call",
						"image_url":  imageUrl,
						"importance": "max",
						"priority":   "high",
						"actions": []map[string]any{
							{
								"type":                 "accept_call_action",
								"text":                 "Accept",
								"shows_user_interface": true,
							},
							{
								"type":                 "decline_call_action",
								"text":                 "Decline",
								"shows_user_interface": false,
							},
						},
						"details": map[string]any{
							"full_screen_intent": true,
						},
					})

					notifScheduler.AddNotification(&ScheduledNotification{
						MulticastMessage: &messaging.MulticastMessage{
							Data: map[string]string{
								"type":           "incoming_call",
								"notification":   string(notifJson),
								"call_type":      callType,
								"invitee":        string(inviteeJson),
								"chat_id":        chat.Id,
								"from_chat_type": fromChatType,
								"image_url":      imageUrl,
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

		e.Router.Add("GET", "/api/room_participants", func(c echo.Context) error {
			// get the room type and chat id
			fromChatType, chatId, err := decodeCallDetailsParams(c)
			if err != nil {
				return err
			}

			// get the chat info
			user := apis.RequestInfo(c).AuthRecord
			room, err := app.Dao().FindFirstRecordByFilter("call_rooms", "from_chat={:from_chat} && invited_participants~{:user}",
				dbx.Params{
					"from_chat": makeChatIdentifier(fromChatType, chatId),
					"user":      user.Id,
				})
			if err != nil {
				return apis.NewNotFoundError("room not found", nil)
			}

			participantsInfo := []map[string]any{}
			if err := apis.EnrichRecord(c, app.Dao(), room, "invited_participants"); err == nil {
				invitedParticipants := room.ExpandedAll("invited_participants")
				participantsByType := map[string][]*models.Record{}

				for _, participant := range invitedParticipants {
					userType := participant.GetString("label")
					participantsByType[userType] = append(participantsByType[userType], participant)
				}

				for pType, participants := range participantsByType {
					collectionName := ""
					switch pType {
					case "parent":
						collectionName = "users_parent"
					case "community":
						collectionName = "users_community"
					default:
						continue
					}

					// fetch the participants
					ids := make([]string, len(participants))
					for idx, participant := range participants {
						ids[idx] = fmt.Sprintf("users = '%s'", participant.Id)
					}

					found, err := app.Dao().FindRecordsByFilter(collectionName, strings.Join(ids, " || "), "", 0, 0)
					if err != nil {
						return err
					}

					for _, participant := range found {
						newRecord := map[string]any{
							"id":             participant.Id,
							"collectionId":   participant.Collection().Id,
							"collectionName": participant.Collection().Name,
							"updated":        "",
							"created":        "",
							"avatar":         participant.GetString("avatar"),
						}

						switch pType {
						case "parent":
							newRecord["name"] = fmt.Sprintf("%s %s %s", participant.GetString("first_name"), participant.GetString("middle_name"), participant.GetString("last_name"))
						case "community":
							newRecord["name"] = participant.GetString("name")
						}

						participantsInfo = append(participantsInfo, newRecord)
					}
				}
			}

			return c.JSON(http.StatusOK, participantsInfo)
		}, apis.RequireRecordAuth())

		// this route is for the invited participants to respond the call
		e.Router.Add("POST", "/api/room_data", func(c echo.Context) error {
			fromChatType, chatId, err := decodeCallDetailsParams(c)
			if err != nil {
				return err
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
			fromChatType, chatId, err := decodeCallDetailsParams(c)
			if err != nil {
				return err
			}

			fromError := c.QueryParam("from_error") == "1"
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

	app.OnRecordAfterUpdateRequest().Add(func(e *core.RecordUpdateEvent) error {
		if e.Collection.Name != "chat_list_ds" && e.Collection.Name != "chat_list_parent" && e.Collection.Name != "chat_list_gc" {
			return nil
		}

		// check if chat list is present in call rooms
		idToLook := makeChatIdentifierRecord(e.Record)
		room, err := app.Dao().FindFirstRecordByFilter("call_rooms", "from_chat={:from_chat}", dbx.Params{"from_chat": idToLook})
		if err != nil {
			return nil
		}

		// if chat list is present, update the invited participants
		invitedParticipants := room.GetStringSlice("invited_participants")

		if e.Collection.Name == "chat_list_parent" {
			if err := apis.EnrichRecord(e.HttpContext, app.Dao(), e.Record, "parents"); err != nil {
				log.Println(err)
				return nil
			}

			expandedParents := e.Record.ExpandedAll("parents")
			parentUsersId := make([]string, len(expandedParents))
			for idx, parent := range expandedParents {
				parentUsersId[idx] = parent.GetString("users")
			}

			invitedParticipants = parentUsersId
		} else {
			if err := apis.EnrichRecord(e.HttpContext, app.Dao(), e.Record, "chatRequestTo", "chatRequestBy"); err != nil {
				log.Println(err)
				return nil
			}

			invitedParticipants = []string{
				e.Record.ExpandedOne("chatRequestTo").GetString("users"),
				e.Record.ExpandedOne("chatRequestBy").GetString("users"),
			}
		}

		room.Set("invited_participants", invitedParticipants)
		if err := app.Dao().SaveRecord(room); err != nil {
			// do not return error or it will cause the update to fail
			log.Println(err)
		}

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatalln(err)
	}
}
