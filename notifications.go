package main

import (
	"context"
	"log"
	"sync"
	"time"

	"firebase.google.com/go/v4/messaging"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

var maxConcurrentNotifications = 3600
var notificationSem = make(chan struct{}, maxConcurrentNotifications)

type ScheduledNotification struct {
	Id               string
	Message          *messaging.Message
	MulticastMessage *messaging.MulticastMessage
	ScheduledTime    time.Time
	CompletionStatus bool
}

type NotificationScheduler struct {
	mutex           sync.Mutex
	MessagingClient *messaging.Client
	Notifier        chan<- *ScheduledNotification
	Notifs          map[string]*ScheduledNotification
}

func NewNotificationScheduler(notifier chan<- *ScheduledNotification) *NotificationScheduler {
	return &NotificationScheduler{
		Notifier: notifier,
		Notifs:   make(map[string]*ScheduledNotification),
	}
}

func (n *NotificationScheduler) AddNotification(notif *ScheduledNotification) {
	id, _ := gonanoid.New()
	notif.Id = id

	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.Notifs[notif.Id] = notif
	go n.monitorAndSend(notif)
}

func (n *NotificationScheduler) monitorAndSend(notif *ScheduledNotification) {
	now := time.Now()
	if notif.ScheduledTime.After(now) {
		time.Sleep(notif.ScheduledTime.Sub(now))
	}

	// acquire the semaphore to limit the number of concurrent notifications
	notificationSem <- struct{}{}

	n.Notifier <- notif

	n.mutex.Lock()
	defer n.mutex.Unlock()

	notif.CompletionStatus = true

	// release the semaphore
	<-notificationSem
}

func (n *NotificationScheduler) RemoveNotification(target string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	delete(n.Notifs, target)
}

func startSchedulingNotifications() (*NotificationScheduler, func()) {
	notifier := make(chan *ScheduledNotification)
	scheduler := NewNotificationScheduler(notifier)
	monitorFunc := func() {
		for notif := range notifier {
			// send the notification
			if notif.Message != nil {
				log.Default().Printf("Sending notification to %s\n", notif.Message.Token)
				_, err := scheduler.MessagingClient.Send(context.Background(), notif.Message)
				if err != nil {
					log.Default().Printf("Error sending notification to %s: %v\n", notif.Id, err)
				}
			} else if notif.MulticastMessage != nil {
				log.Default().Printf("Sending notification to %q\n", notif.MulticastMessage.Tokens)
				_, err := scheduler.MessagingClient.SendEachForMulticast(context.Background(), notif.MulticastMessage)
				if err != nil {
					log.Default().Printf("Error sending notification to %s: %v\n", notif.Id, err)
				}
			} else {
				log.Default().Printf("Error sending notification to %s: no message specified\n", notif.Id)
			}

			scheduler.RemoveNotification(notif.Id)
		}
	}

	return scheduler, monitorFunc
}
