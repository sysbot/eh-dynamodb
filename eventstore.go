// Copyright (c) 2018 - The Event Horizon DynamoDB authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/google/uuid"
	"github.com/guregu/dynamo"
	eh "github.com/looplab/eventhorizon"
)

// ErrCouldNotClearDB is when the database could not be cleared.
var ErrCouldNotClearDB = errors.New("could not clear database")

// ErrCouldNotMarshalEvent is when an event could not be marshaled into BSON.
var ErrCouldNotMarshalEvent = errors.New("could not marshal event")

// ErrCouldNotUnmarshalEvent is when an event could not be unmarshaled into a concrete type.
var ErrCouldNotUnmarshalEvent = errors.New("could not unmarshal event")

// ErrCouldNotSaveAggregate is when an aggregate could not be saved.
var ErrCouldNotSaveAggregate = errors.New("could not save aggregate")

// EventStore implements an EventStore for DynamoDB.
type EventStore struct {
	tablePrefix  string
	service      *dynamo.DB
	eventHandler eh.EventHandler
	tableName    func(context.Context) string
}

// Option is an option setter used to configure creation.
type Option func(*EventStore) error

// WithEventHandler adds an event handler that will be called when saving events.
// An example would be to add an event bus to publish events.
func WithEventHandler(h eh.EventHandler) Option {
	return func(s *EventStore) error {
		s.eventHandler = h
		return nil
	}
}

// WithDBName uses a custom DB name function.
func WithDynamoDB(sess *session.Session) Option {
	return func(r *EventStore) error {
		r.service = dynamo.New(sess)
		return nil
	}
}

// NewEventStore creates a new EventStore.
func NewEventStore(tablePrefix string, options ...Option) (*EventStore, error) {
	awsConfig := &aws.Config{
		Region:   aws.String("us-west-2"),
		Endpoint: aws.String("http://localhost:8000"),
	}

	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, ErrCouldNotDialDB
	}

	s := &EventStore{
		tablePrefix: "eventhorizonEvents",
		service:     dynamo.New(sess),
	}

	s.tableName = func(ctx context.Context) string {
		ns := eh.NamespaceFromContext(ctx)
		return tablePrefix + "_" + ns
	}

	for _, option := range options {
		if err := option(s); err != nil {
			return nil, fmt.Errorf("error while applying option: %v", err)
		}
	}

	return s, nil
}

// Save implements the Save method of the eventhorizon.EventStore interface.
func (s *EventStore) Save(ctx context.Context, events []eh.Event, originalVersion int) error {
	if len(events) == 0 {
		return eh.EventStoreError{
			Err:       eh.ErrNoEventsToAppend,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	// Build all event records, with incrementing versions starting from the
	// original aggregate version.
	aggregateID := events[0].AggregateID()
	version := originalVersion
	table := s.service.Table(s.tableName(ctx))
	for _, event := range events {
		// Only accept events belonging to the same aggregate.
		if event.AggregateID() != aggregateID {
			return eh.EventStoreError{
				Err:       eh.ErrInvalidEvent,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}

		// Only accept events that apply to the correct aggregate version.
		if event.Version() != version+1 {
			return eh.EventStoreError{
				Err:       eh.ErrIncorrectEventVersion,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}

		// Create the event record for the DB.
		e, err := newDBEvent(ctx, event)
		if err != nil {
			return err
		}
		version++

		// TODO: Implement atomic version counter for the aggregate.
		// TODO: Batch write all events.
		// TODO: Support translating not found to not be an error but an
		// empty list.
		if err := table.Put(e).If("attribute_not_exists(AggregateID) AND attribute_not_exists(Version)").Run(); err != nil {
			if err, ok := err.(awserr.RequestFailure); ok && err.Code() == "ConditionalCheckFailedException" {
				return eh.EventStoreError{
					BaseErr:   err,
					Err:       ErrCouldNotSaveAggregate,
					Namespace: eh.NamespaceFromContext(ctx),
				}
			}
			return eh.EventStoreError{
				BaseErr:   err,
				Err:       err,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}
	}

	// Let the optional event handler handle the events. Aborts the transaction
	// in case of error.
	if s.eventHandler != nil {
		for _, e := range events {
			if err := s.eventHandler.HandleEvent(ctx, e); err != nil {
				return eh.CouldNotHandleEventError{
					Err:       err,
					Event:     e,
					Namespace: eh.NamespaceFromContext(ctx),
				}
			}
		}
	}

	return nil
}

// Load implements the Load method of the eventhorizon.EventStore interface.
func (s *EventStore) Load(ctx context.Context, id uuid.UUID) ([]eh.Event, error) {
	table := s.service.Table(s.tableName(ctx))

	var dbEvents []dbEvent
	err := table.Get("AggregateID", id.String()).Consistent(true).All(&dbEvents)
	if err, ok := err.(awserr.RequestFailure); ok && err.Code() == "ResourceNotFoundException" {
		return []eh.Event{}, nil
	} else if err != nil {
		return nil, eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return s.buildEvents(ctx, dbEvents)
}

// LoadAll will load all the events from the event store (useful to replay events)
func (s *EventStore) LoadAll(ctx context.Context) ([]eh.Event, error) {
	table := s.service.Table(s.tableName(ctx))

	var dbEvents []dbEvent
	err := table.Scan().Consistent(true).All(&dbEvents)
	if err != nil {
		return nil, eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return s.buildEvents(ctx, dbEvents)
}

func (s *EventStore) buildEvents(ctx context.Context, dbEvents []dbEvent) ([]eh.Event, error) {
	events := make([]eh.Event, len(dbEvents))
	for i, dbEvent := range dbEvents {

		// Create an event of the correct type.
		if data, err := eh.CreateEventData(dbEvent.EventType); err == nil {
			// Manually decode the raw event.
			if err := dynamodbattribute.UnmarshalMap(dbEvent.RawData, data); err != nil {
				return nil, eh.EventStoreError{
					BaseErr:   err,
					Err:       ErrCouldNotUnmarshalEvent,
					Namespace: eh.NamespaceFromContext(ctx),
				}
			}

			// Set concrete event and zero out the decoded event.
			dbEvent.data = data
			dbEvent.RawData = nil
		}

		events[i] = event{dbEvent: dbEvent}
	}

	return events, nil
}

// Replace implements the Replace method of the eventhorizon.EventStore interface.
func (s *EventStore) Replace(ctx context.Context, event eh.Event) error {
	table := s.service.Table(s.tableName(ctx))

	count, err := table.Get("AggregateID", event.AggregateID().String()).Consistent(true).Count()
	if err != nil {
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	} else if count == 0 {
		return eh.ErrAggregateNotFound
	}

	// Create the event record for the DB.
	e, err := newDBEvent(ctx, event)
	if err != nil {
		return err
	}

	if err := table.Put(e).If("attribute_exists(AggregateID) AND attribute_exists(Version)").Run(); err != nil {
		if err, ok := err.(awserr.RequestFailure); ok && err.Code() == "ConditionalCheckFailedException" {
			return eh.ErrInvalidEvent
		}
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// RenameEvent implements the RenameEvent method of the eventhorizon.EventStore interface.
func (s *EventStore) RenameEvent(ctx context.Context, from, to eh.EventType) error {
	table := s.service.Table(s.tableName(ctx))

	var dbEvents []dbEvent
	err := table.Scan().Filter("EventType = ?", from).Consistent(true).All(&dbEvents)
	if err != nil {
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	for _, dbEvent := range dbEvents {
		if err := table.Update("AggregateID", dbEvent.AggregateID).Range("Version", dbEvent.Version).If("EventType = ?", from).Set("EventType", to).Run(); err != nil {
			return eh.EventStoreError{
				BaseErr:   err,
				Err:       err,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}
	}

	return nil
}

// CreateTable creates the table if it is not already existing and correct.
func (s *EventStore) CreateTable(ctx context.Context) error {
	if err := s.service.CreateTable(s.tableName(ctx), dbEvent{}).Run(); err != nil {
		return err
	}

	describeParams := &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName(ctx)),
	}
	if err := s.service.Client().WaitUntilTableExists(describeParams); err != nil {
		return err
	}

	return nil
}

// DeleteTable deletes the event table.
func (s *EventStore) DeleteTable(ctx context.Context) error {
	table := s.service.Table(s.tableName(ctx))
	err := table.DeleteTable().Run()
	if err != nil {
		if err, ok := err.(awserr.RequestFailure); ok && err.Code() == "ResourceNotFoundException" {
			return nil
		}
		return ErrCouldNotClearDB
	}

	describeParams := &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName(ctx)),
	}
	if err := s.service.Client().WaitUntilTableNotExists(describeParams); err != nil {
		return err
	}

	return nil
}

// dbEvent is the internal event record for the DynamoDB event store used
// to save and load events from the DB.
type dbEvent struct {
	AggregateID uuid.UUID `dynamo:",hash"`
	Version     int       `dynamo:",range"`

	EventType     eh.EventType
	RawData       map[string]*dynamodb.AttributeValue
	data          eh.EventData
	Timestamp     time.Time
	AggregateType eh.AggregateType
	Metadata      map[string]interface{}
}

// newDBEvent returns a new dbEvent for an event.
func newDBEvent(ctx context.Context, event eh.Event) (*dbEvent, error) {
	// Marshal event data if there is any.
	var rawData map[string]*dynamodb.AttributeValue
	if event.Data() != nil {
		var err error
		rawData, err = dynamodbattribute.MarshalMap(event.Data())
		if err != nil {
			return nil, eh.EventStoreError{
				BaseErr:   err,
				Err:       ErrCouldNotMarshalEvent,
				Namespace: eh.NamespaceFromContext(ctx),
			}
		}
	}

	return &dbEvent{
		EventType:     event.EventType(),
		RawData:       rawData,
		Timestamp:     event.Timestamp(),
		AggregateType: event.AggregateType(),
		AggregateID:   event.AggregateID(),
		Version:       event.Version(),
		Metadata:      event.Metadata(),
	}, nil
}

// event is the private implementation of the eventhorizon.Event
// interface for a DynamoDB event store.
type event struct {
	dbEvent
}

// Metadata implements the Metadata method of the Event interface.
func (e event) Metadata() map[string]interface{} {
	return e.dbEvent.Metadata
}

// EventType implements the EventType method of the eventhorizon.Event interface.
func (e event) EventType() eh.EventType {
	return e.dbEvent.EventType
}

// Data implements the Data method of the eventhorizon.Event interface.
func (e event) Data() eh.EventData {
	return e.dbEvent.data
}

// Timestamp implements the Timestamp method of the eventhorizon.Event interface.
func (e event) Timestamp() time.Time {
	return e.dbEvent.Timestamp
}

// AggregateType implements the AggregateType method of the eventhorizon.Event interface.
func (e event) AggregateType() eh.AggregateType {
	return e.dbEvent.AggregateType
}

// AggregateID implements the AggregateID method of the eventhorizon.Event interface.
func (e event) AggregateID() uuid.UUID {
	return uuid.UUID(e.dbEvent.AggregateID)
}

// Version implements the Version method of the eventhorizon.Event interface.
func (e event) Version() int {
	return e.dbEvent.Version
}

// String implements the String method of the eventhorizon.Event interface.
func (e event) String() string {
	return fmt.Sprintf("%s@%d", e.dbEvent.EventType, e.dbEvent.Version)
}
