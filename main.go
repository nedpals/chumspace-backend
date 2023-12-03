package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/labstack/echo/v5"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/models"
	"github.com/pocketbase/pocketbase/tools/security"
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

func main() {
	app := pocketbase.New()

	// serves static files from the provided public dir (if exists)
	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		firebaseApp := initializeFirebase()
		messagingClient, err := firebaseApp.Messaging(context.Background())
		if err != nil {
			return err
		}

		// get the livekit api key and secret first
		lkApiKey, lkApiKeyExists := os.LookupEnv("LIVEKIT_API_KEY")
		if !lkApiKeyExists {
			return apis.NewApiError(
				http.StatusInternalServerError,
				"Unable to fulfill your request at this time due to an internal error.",
				nil)
		}

		lkApiSecret, lkApiSecretExists := os.LookupEnv("LIVEKIT_API_SECRET")
		if !lkApiSecretExists {
			return apis.NewApiError(
				http.StatusInternalServerError,
				"Unable to fulfill your request at this time due to an internal error.",
				nil)
		}

		e.Router.Add("POST", "/api/join_call", func(c echo.Context) error {
			// get the call type
			callType := c.QueryParamDefault("type", "audio")

			// get the chat info
			chatId := c.QueryParam("chat_id")
			if len(chatId) == 0 {
				return apis.NewBadRequestError("chat_id is required", nil)
			}

			chat, err := app.Dao().FindRecordById("chat_list_parent", chatId)
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

			// get the room if it already exists
			roomRecord, err := app.Dao().FindFirstRecordByFilter(roomCollection.Id, "chat={:chat}", dbx.Params{"chat": chat.Id})
			if err != nil {
				// create the room if it doesn't exist
				roomRecord = models.NewRecord(roomCollection)
				roomRecord.Set("chat", chat.Id)
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
			claims := map[string]any{
				"iss":      lkApiKey,
				"sub":      identity,
				"room":     roomRecord.Id,
				"name":     participantName,
				"metadata": user.Id,
				"video": map[string]bool{
					"roomJoin":     true,
					"canPublish":   true,
					"canSubscribe": true,
				},
			}

			// create a JWT token
			token, err := security.NewJWT(claims, lkApiSecret, 6*60*60)
			if err != nil {
				return err
			}

			// notify other invited participants
			if !isRoomExisting {
				inviteeJson, _ := json.Marshal(user.PublicExport())
				invitedParticipants := roomRecord.GetStringSlice("invited_participants")
				tokens := []string{}

				// get the tokens of the invited participants
				for _, participantId := range invitedParticipants {
					if participantId == user.Id {
						continue
					}
					participant, err := app.Dao().FindRecordById("users", participantId)
					if err != nil {
						continue
					}
					participantTokens := []string{}
					if err := participant.UnmarshalJSONField("tokens", &participantTokens); err != nil {
						continue
					}

					tokens = append(tokens, participantTokens...)
				}

				if len(tokens) != 0 {
					// construct the message
					message := &messaging.MulticastMessage{
						Data: map[string]string{
							"type":      "incoming_call",
							"call_type": callType,
							"invitee":   string(inviteeJson),
							"chat_id":   chat.Id,
						},
						Notification: &messaging.Notification{
							Title:    "Incoming Call",
							Body:     participantName + " is inviting you to a call",
							ImageURL: fmt.Sprintf("%s/api/files/users/%s/%s", app.Settings().Meta.AppUrl, user.Id, user.GetString("avatar")),
						},
						Android: &messaging.AndroidConfig{
							Priority: "high",
						},
						Tokens: tokens,
					}

					_, err := messagingClient.SendEachForMulticast(c.Request().Context(), message)
					if err != nil {
						log.Println(err)
					}
				}
			}

			// return the token
			return c.JSON(http.StatusOK, map[string]string{
				"token": token,
				"room":  roomRecord.Id,
			})
		}, apis.RequireRecordAuth())

		e.Router.Add("POST", "/api/leave_call", func(c echo.Context) error {
			// get the chat info
			fromError := c.QueryParam("from_error") == "1"
			chatId := c.QueryParam("chat_id")
			if len(chatId) == 0 {
				return apis.NewBadRequestError("chat_id is required", nil)
			}

			user := apis.RequestInfo(c).AuthRecord
			room, err := app.Dao().FindFirstRecordByFilter("call_rooms", "chat={:chat} && participants~{:user}", dbx.Params{"chat": chatId, "user": user.Id})
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

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatalln(err)
	}
}
