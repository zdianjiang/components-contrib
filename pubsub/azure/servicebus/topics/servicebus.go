/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package topics

import (
	"context"
	"errors"
	"time"

	servicebus "github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/cenkalti/backoff/v4"

	impl "github.com/dapr/components-contrib/internal/component/azure/servicebus"
	"github.com/dapr/components-contrib/internal/utils"
	contribMetadata "github.com/dapr/components-contrib/metadata"
	"github.com/dapr/components-contrib/pubsub"
	"github.com/dapr/kit/logger"
	"github.com/dapr/kit/retry"
)

const (
	defaultMaxBulkSubCount        = 100
	defaultMaxBulkPubBytes uint64 = 1024 * 128 // 128 KiB
)

type azureServiceBus struct {
	metadata *impl.Metadata
	client   *impl.Client
	logger   logger.Logger
	features []pubsub.Feature
}

// NewAzureServiceBusTopics returns a new pub-sub implementation.
func NewAzureServiceBusTopics(logger logger.Logger) pubsub.PubSub {
	return &azureServiceBus{
		logger:   logger,
		features: []pubsub.Feature{pubsub.FeatureMessageTTL},
	}
}

func (a *azureServiceBus) Init(metadata pubsub.Metadata) (err error) {
	a.metadata, err = impl.ParseMetadata(metadata.Properties, a.logger, impl.MetadataModeTopics)
	if err != nil {
		return err
	}

	a.client, err = impl.NewClient(a.metadata, metadata.Properties)
	if err != nil {
		return err
	}

	return nil
}

func (a *azureServiceBus) Publish(ctx context.Context, req *pubsub.PublishRequest) error {
	msg, err := impl.NewASBMessageFromPubsubRequest(req)
	if err != nil {
		return err
	}

	ebo := backoff.NewExponentialBackOff()
	ebo.InitialInterval = time.Duration(a.metadata.PublishInitialRetryIntervalInMs) * time.Millisecond
	bo := backoff.WithMaxRetries(ebo, uint64(a.metadata.PublishMaxRetries))
	bo = backoff.WithContext(bo, ctx)

	msgID := "nil"
	if msg.MessageID != nil {
		msgID = *msg.MessageID
	}
	return retry.NotifyRecover(
		func() (err error) {
			// Ensure the queue or topic exists the first time it is referenced
			// This does nothing if DisableEntityManagement is true
			err = a.client.EnsureTopic(ctx, req.Topic)
			if err != nil {
				return err
			}

			// Get the sender
			var sender *servicebus.Sender
			sender, err = a.client.GetSender(ctx, req.Topic)
			if err != nil {
				return err
			}

			// Try sending the message
			publishCtx, publishCancel := context.WithTimeout(ctx, time.Second*time.Duration(a.metadata.TimeoutInSec))
			err = sender.SendMessage(publishCtx, msg, nil)
			publishCancel()
			if err != nil {
				if impl.IsNetworkError(err) {
					// Retry after reconnecting
					a.client.CloseSender(req.Topic)
					return err
				}

				if impl.IsRetriableAMQPError(err) {
					// Retry (no need to reconnect)
					return err
				}

				// Do not retry on other errors
				return backoff.Permanent(err)
			}
			return nil
		},
		bo,
		func(err error, _ time.Duration) {
			a.logger.Warnf("Could not publish service bus message (%s). Retrying...: %v", msgID, err)
		},
		func() {
			a.logger.Infof("Successfully published service bus message (%s) after it previously failed", msgID)
		},
	)
}

func (a *azureServiceBus) BulkPublish(ctx context.Context, req *pubsub.BulkPublishRequest) (pubsub.BulkPublishResponse, error) {
	// If the request is empty, sender.SendMessageBatch will panic later.
	// Return an empty response to avoid this.
	if len(req.Entries) == 0 {
		a.logger.Warnf("Empty bulk publish request, skipping")
		return pubsub.BulkPublishResponse{}, nil
	}

	// Ensure the queue or topic exists the first time it is referenced
	// This does nothing if DisableEntityManagement is true
	err := a.client.EnsureTopic(ctx, req.Topic)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, err), err
	}

	// Get the sender
	sender, err := a.client.GetSender(ctx, req.Topic)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, err), err
	}

	// Create a new batch of messages with batch options.
	batchOpts := &servicebus.MessageBatchOptions{
		MaxBytes: utils.GetElemOrDefaultFromMap(req.Metadata, contribMetadata.MaxBulkPubBytesKey, defaultMaxBulkPubBytes),
	}

	batchMsg, err := sender.NewMessageBatch(ctx, batchOpts)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, err), err
	}

	// Add messages from the bulk publish request to the batch.
	err = impl.UpdateASBBatchMessageWithBulkPublishRequest(batchMsg, req)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, err), err
	}

	// Azure Service Bus does not return individual status for each message in the request.
	err = sender.SendMessageBatch(ctx, batchMsg, nil)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, err), err
	}

	return pubsub.BulkPublishResponse{}, nil
}

func (a *azureServiceBus) Subscribe(subscribeCtx context.Context, req pubsub.SubscribeRequest, handler pubsub.Handler) error {
	requireSessions := utils.IsTruthy(req.Metadata[impl.RequireSessionsMetadataKey])
	sessionIdleTimeout := time.Duration(utils.GetElemOrDefaultFromMap(req.Metadata, impl.SessionIdleTimeoutMetadataKey, impl.DefaultSesssionIdleTimeoutInSec)) * time.Second
	maxConcurrentSessions := utils.GetElemOrDefaultFromMap(req.Metadata, impl.MaxConcurrentSessionsMetadataKey, impl.DefaultMaxConcurrentSessions)

	sub := impl.NewSubscription(
		subscribeCtx, impl.SubsriptionOptions{
			MaxActiveMessages:     a.metadata.MaxActiveMessages,
			TimeoutInSec:          a.metadata.TimeoutInSec,
			MaxBulkSubCount:       nil,
			MaxRetriableEPS:       a.metadata.MaxRetriableErrorsPerSec,
			MaxConcurrentHandlers: a.metadata.MaxConcurrentHandlers,
			Entity:                "topic " + req.Topic,
			LockRenewalInSec:      a.metadata.LockRenewalInSec,
			RequireSessions:       requireSessions,
		},
		a.logger,
	)

	receiveAndBlockFn := func(receiver impl.Receiver, onFirstSuccess func()) error {
		return sub.ReceiveBlocking(
			impl.GetPubSubHandlerFunc(req.Topic, handler, a.logger, time.Duration(a.metadata.HandlerTimeoutInSec)*time.Second),
			receiver,
			onFirstSuccess,
			impl.ReceiveOptions{
				BulkEnabled:        false, // Bulk is not supported in regular Subscribe.
				SessionIdleTimeout: sessionIdleTimeout,
			},
		)
	}

	return a.doSubscribe(subscribeCtx, req, sub, receiveAndBlockFn, impl.SubscribeOptions{
		RequireSessions:      requireSessions,
		MaxConcurrentSesions: maxConcurrentSessions,
	})
}

func (a *azureServiceBus) BulkSubscribe(subscribeCtx context.Context, req pubsub.SubscribeRequest, handler pubsub.BulkHandler) error {
	requireSessions := utils.IsTruthy(req.Metadata[impl.RequireSessionsMetadataKey])
	sessionIdleTimeout := time.Duration(utils.GetElemOrDefaultFromMap(req.Metadata, impl.SessionIdleTimeoutMetadataKey, impl.DefaultSesssionIdleTimeoutInSec)) * time.Second
	maxConcurrentSessions := utils.GetElemOrDefaultFromMap(req.Metadata, impl.MaxConcurrentSessionsMetadataKey, impl.DefaultMaxConcurrentSessions)

	maxBulkSubCount := utils.GetIntValOrDefault(req.BulkSubscribeConfig.MaxMessagesCount, defaultMaxBulkSubCount)
	sub := impl.NewSubscription(
		subscribeCtx, impl.SubsriptionOptions{
			MaxActiveMessages:     a.metadata.MaxActiveMessages,
			TimeoutInSec:          a.metadata.TimeoutInSec,
			MaxBulkSubCount:       &maxBulkSubCount,
			MaxRetriableEPS:       a.metadata.MaxRetriableErrorsPerSec,
			MaxConcurrentHandlers: a.metadata.MaxConcurrentHandlers,
			Entity:                "topic " + req.Topic,
			LockRenewalInSec:      a.metadata.LockRenewalInSec,
			RequireSessions:       requireSessions,
		},
		a.logger,
	)

	receiveAndBlockFn := func(receiver impl.Receiver, onFirstSuccess func()) error {
		return sub.ReceiveBlocking(
			impl.GetBulkPubSubHandlerFunc(req.Topic, handler, a.logger, time.Duration(a.metadata.HandlerTimeoutInSec)*time.Second),
			receiver,
			onFirstSuccess,
			impl.ReceiveOptions{
				BulkEnabled:        true, // Bulk is supported in BulkSubscribe.
				SessionIdleTimeout: sessionIdleTimeout,
			},
		)
	}

	return a.doSubscribe(subscribeCtx, req, sub, receiveAndBlockFn, impl.SubscribeOptions{
		RequireSessions:      requireSessions,
		MaxConcurrentSesions: maxConcurrentSessions,
	})
}

// doSubscribe is a helper function that handles the common logic for both Subscribe and BulkSubscribe.
// The receiveAndBlockFn is a function should invoke a blocking call to receive messages from the topic.
func (a *azureServiceBus) doSubscribe(subscribeCtx context.Context,
	req pubsub.SubscribeRequest, sub *impl.Subscription, receiveAndBlockFn func(impl.Receiver, func()) error, opts impl.SubscribeOptions,
) error {
	// Does nothing if DisableEntityManagement is true
	err := a.client.EnsureSubscription(subscribeCtx, a.metadata.ConsumerID, req.Topic, opts)
	if err != nil {
		return err
	}

	// Reconnection backoff policy
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0
	bo.InitialInterval = time.Duration(a.metadata.MinConnectionRecoveryInSec) * time.Second
	bo.MaxInterval = time.Duration(a.metadata.MaxConnectionRecoveryInSec) * time.Second

	onFirstSuccess := func() {
		// Reset the backoff when the subscription is successful and we have received the first message
		bo.Reset()
	}

	go func() {
		// Reconnect loop.
		for {
			if opts.RequireSessions {
				a.ConnectAndReceiveWithSessions(subscribeCtx, req, sub, receiveAndBlockFn, onFirstSuccess, opts.MaxConcurrentSesions)
			} else {
				a.ConnectAndReceive(subscribeCtx, req, sub, receiveAndBlockFn, onFirstSuccess)
			}

			// If context was canceled, do not attempt to reconnect
			if subscribeCtx.Err() != nil {
				a.logger.Debug("Context canceled; will not reconnect")
				return
			}

			wait := bo.NextBackOff()
			a.logger.Warnf("Subscription to topic %s lost connection, attempting to reconnect in %s...", req.Topic, wait)
			time.Sleep(wait)
		}
	}()

	return nil
}

func (a *azureServiceBus) Close() (err error) {
	a.client.CloseAllSenders(a.logger)
	return nil
}

func (a *azureServiceBus) Features() []pubsub.Feature {
	return a.features
}

func (a *azureServiceBus) ConnectAndReceive(subscribeCtx context.Context, req pubsub.SubscribeRequest, sub *impl.Subscription, receiveAndBlockFn func(impl.Receiver, func()) error, onFirstSuccess func()) error {
	defer func() {
		// Gracefully close the connection (in case it's not closed already)
		// Use a background context here (with timeout) because ctx may be closed already.
		closeSubCtx, closeSubCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
		sub.Close(closeSubCtx)
		closeSubCancel()
	}()

	// Blocks until a successful connection (or until context is canceled)
	receiver, err := sub.Connect(func() (impl.Receiver, error) {
		a.logger.Debugf("Connecting to subscription %s for topic %s", a.metadata.ConsumerID, req.Topic)
		r, err := a.client.GetClient().NewReceiverForSubscription(req.Topic, a.metadata.ConsumerID, nil)
		return impl.NewMessageReceiver(r), err
	})
	if err != nil {
		// Realistically, the only time we should get to this point is if the context was canceled, but let's log any other error we may get.
		if !errors.Is(err, context.Canceled) {
			a.logger.Errorf("Could not instantiate session subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
		}
		return nil
	}

	lockCtx, lockCancel := context.WithCancel(subscribeCtx)
	defer func() {
		// Cancel the lock renewal loop
		lockCancel()

		// Close the receiver
		a.logger.Debugf("Closing message receiver for subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
		closeReceiverCtx, closeReceiverCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
		receiver.Close(closeReceiverCtx)
		closeReceiverCancel()
	}()

	// lock renewal loop
	go func() {
		a.logger.Debugf("Renewing locks for subscription %s for topic %s", a.metadata.ConsumerID, req.Topic)
		lockErr := sub.RenewLocksBlocking(lockCtx, receiver, impl.LockRenewalOptions{
			RenewalInSec: a.metadata.LockRenewalInSec,
			TimeoutInSec: a.metadata.TimeoutInSec,
		})
		if lockErr != nil {
			if !errors.Is(lockErr, context.Canceled) {
				a.logger.Error(lockErr)
			}
		}
	}()

	a.logger.Debugf("Receiving messages for subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)

	// receiveAndBlockFn will only return with an error that it cannot handle internally. The subscription connection is closed when this method returns.
	// If that occurs, we will log the error and attempt to re-establish the subscription connection until we exhaust the number of reconnect attempts.
	if err := receiveAndBlockFn(receiver, onFirstSuccess); err != nil {
		return err
	}

	return nil
}

func (a *azureServiceBus) ConnectAndReceiveWithSessions(subscribeCtx context.Context, req pubsub.SubscribeRequest, sub *impl.Subscription, receiveAndBlockFn func(impl.Receiver, func()) error, onFirstSuccess func(), maxConcurrentSessions int) {
	sessionsChan := make(chan struct{}, maxConcurrentSessions)
	for i := 0; i < maxConcurrentSessions; i++ {
		sessionsChan <- struct{}{}
	}

	defer func() {
		// Gracefully close the connection (in case it's not closed already)
		// Use a background context here (with timeout) because ctx may be closed already
		closeSubCtx, closeSubCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
		sub.Close(closeSubCtx)
		closeSubCancel()
	}()

	for {
		select {
		case <-subscribeCtx.Done():
			return
		case <-sessionsChan:
			select {
			case <-subscribeCtx.Done():
				return
			default:
				func() { // IIFE to scope context cancellation
					acceptCtx, acceptCancel := context.WithCancel(subscribeCtx)
					defer acceptCancel()

					var sessionID string

					// Blocks until a successful connection (or until context is canceled)
					receiver, err := sub.Connect(func() (impl.Receiver, error) {
						a.logger.Debugf("Accepting next available session subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
						r, err := a.client.GetClient().AcceptNextSessionForSubscription(acceptCtx, req.Topic, a.metadata.ConsumerID, nil)
						if err == nil && r != nil {
							sessionID = r.SessionID()
						}
						return impl.NewSessionReceiver(r), err
					})
					if err != nil {
						// Realistically, the only time we should get to this point is if the context was canceled, but let's log any other error we may get.
						if !errors.Is(err, context.Canceled) {
							a.logger.Errorf("Could not instantiate session subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
						}
						return
					}

					go func() {
						lockCtx, lockCancel := context.WithCancel(subscribeCtx)
						defer func() {
							// cancel the lock renewal loop
							lockCancel()

							// close the receiver
							a.logger.Debugf("Closing session %s receiver for subscription %s to topic %s", sessionID, a.metadata.ConsumerID, req.Topic)
							closeReceiverCtx, closeReceiverCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
							receiver.Close(closeReceiverCtx)
							closeReceiverCancel()

							// return the session to the pool
							a.logger.Debugf("Returning session to pool")
							sessionsChan <- struct{}{}
						}()

						// lock renewal loop
						go func() {
							a.logger.Debugf("Renewing locks for session %s receiver for subscription %s to topic %s", sessionID, a.metadata.ConsumerID, req.Topic)
							lockErr := sub.RenewLocksBlocking(lockCtx, receiver, impl.LockRenewalOptions{
								RenewalInSec: a.metadata.LockRenewalInSec,
								TimeoutInSec: a.metadata.TimeoutInSec,
							})
							if lockErr != nil {
								if !errors.Is(lockErr, context.Canceled) {
									a.logger.Error(lockErr)
								}
							}
						}()

						a.logger.Debugf("Receiving messages for session %s receiver for subscription %s to topic %s", sessionID, a.metadata.ConsumerID, req.Topic)

						// receiveAndBlockFn will only return with an error that it cannot handle internally. The subscription connection is closed when this method returns.
						// If that occurs, we will log the error and attempt to re-establish the subscription connection until we exhaust the number of reconnect attempts.
						err = receiveAndBlockFn(receiver, onFirstSuccess)
						if err != nil && !errors.Is(err, context.Canceled) {
							a.logger.Error(err)
						}
					}() // end session receive goroutine
				}()
			}
		}
	}
}
