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

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/google/uuid"
	"github.com/guregu/dynamo"
	eh "github.com/looplab/eventhorizon"
)

// ErrCouldNotDialDB is when the database could not be dialed.
var ErrCouldNotDialDB = errors.New("could not dial database")

// ErrModelNotSet is when an model factory is not set on the Repo.
var ErrModelNotSet = errors.New("model not set")

// Repo implements a DynamoDB repository for entities.
type Repo struct {
	tablePrefix string
	service     *dynamo.DB
	factoryFn   func() eh.Entity
	tableName   func(context.Context) string
}

// Option is an option setter used to configure creation.
type OptionRepo func(*Repo) error

// WithPrefixAsDBName uses only the prefix as DB name, without namespace support.
func WithRepoPrefixAsTableName() OptionRepo {
	return func(r *Repo) error {
		r.tableName = func(context.Context) string {
			return r.tablePrefix
		}
		return nil
	}
}

// WithDBName uses a custom DB name function.
func WithRepoTableName(tableName func(context.Context) string) OptionRepo {
	return func(r *Repo) error {
		r.tableName = tableName
		return nil
	}
}

// WithRepoDBName uses a custom DB name function.
func WithRepoDynamoDB(sess *session.Session) OptionRepo {
	return func(r *Repo) error {
		r.service = dynamo.New(sess)
		return nil
	}
}

func WithRepoEntityFactoryFunc(f func() eh.Entity) OptionRepo {
	return func(r *Repo) error {
		r.factoryFn = f
		return nil
	}
}

// NewRepo creates a new Repo.
func NewRepo(tablePrefix string, options ...OptionRepo) (*Repo, error) {
	awsConfig := &aws.Config{
		Region:   aws.String("us-west-2"),
		Endpoint: aws.String("http://localhost:8000"),
	}

	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, ErrCouldNotDialDB
	}

	r := &Repo{
		tablePrefix: tablePrefix,
		service:     dynamo.New(sess),
	}

	r.tableName = func(ctx context.Context) string {
		ns := eh.NamespaceFromContext(ctx)
		return tablePrefix + "_" + ns
	}

	for _, option := range options {
		if err := option(r); err != nil {
			return nil, fmt.Errorf("error while applying option: %v", err)
		}
	}

	return r, nil
}

// Parent implements the Parent method of the eventhorizon.ReadRepo interface.
func (r *Repo) Parent() eh.ReadRepo {
	return nil
}

func (r *Repo) CreateTable(ctx context.Context) error {
	if r.service == nil {
		return ErrCouldNotDialDB
	}
	if r.factoryFn == nil {
		return ErrModelNotSet
	}

	if err := r.service.CreateTable(r.tableName(ctx), r.factoryFn()).Run(); err != nil {
		return err
	}

	describeParams := &dynamodb.DescribeTableInput{
		TableName: aws.String(r.tableName(ctx)),
	}
	if err := r.service.Client().WaitUntilTableExists(describeParams); err != nil {
		return err
	}

	return nil

}

func (r *Repo) DeleteTable(ctx context.Context) error {
	if r.service == nil {
		return ErrCouldNotDialDB
	}

	if err := r.service.Table(r.tableName(ctx)).DeleteTable().RunWithContext(ctx); err != nil {
		if err, ok := err.(awserr.RequestFailure); ok && err.Code() == "ResourceNotFoundException" {
			return nil
		}
		return ErrCouldNotClearDB
	}

	describeParams := &dynamodb.DescribeTableInput{
		TableName: aws.String(r.tableName(ctx)),
	}
	if err := r.service.Client().WaitUntilTableNotExists(describeParams); err != nil {
		return err
	}

	return nil
}

// Find implements the Find method of the eventhorizon.ReadRepo interface.
func (r *Repo) Find(ctx context.Context, id uuid.UUID) (eh.Entity, error) {
	if r.factoryFn == nil {
		return nil, eh.RepoError{
			Err:       ErrModelNotSet,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	table := r.service.Table(r.tableName(ctx))
	entity := r.factoryFn()

	// TODO support range by adding Get().Range() here
	err := table.Get("ID", id.String()).Consistent(true).One(entity)

	if err != nil {
		return nil, eh.RepoError{
			Err:       eh.ErrEntityNotFound,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return entity, nil
}

// FindAll implements the FindAll method of the eventhorizon.ReadRepo interface.
func (r *Repo) FindAll(ctx context.Context) ([]eh.Entity, error) {
	if r.factoryFn == nil {
		return nil, eh.RepoError{
			Err:       ErrModelNotSet,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	table := r.service.Table(r.tableName(ctx))

	iter := table.Scan().Consistent(true).Iter()
	result := []eh.Entity{}
	entity := r.factoryFn()
	for iter.Next(entity) {
		result = append(result, entity)
		entity = r.factoryFn()
	}

	return result, nil
}

// FindWithFilter allows to find entities with a filter
func (r *Repo) FindWithFilter(ctx context.Context, expr string, args ...interface{}) ([]eh.Entity, error) {
	if r.factoryFn == nil {
		return nil, eh.RepoError{
			Err:       ErrModelNotSet,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	table := r.service.Table(r.tableName(ctx))

	iter := table.Scan().Filter(expr, args...).Consistent(true).Iter()
	result := []eh.Entity{}
	entity := r.factoryFn()
	for iter.Next(entity) {
		result = append(result, entity)
		entity = r.factoryFn()
	}

	return result, nil
}

// FindWithFilterUsingIndex allows to find entities with a filter using an index
func (r *Repo) FindWithFilterUsingIndex(ctx context.Context, indexInput IndexInput, filterQuery string, filterArgs ...interface{}) ([]eh.Entity, error) {
	if r.factoryFn == nil {
		return nil, eh.RepoError{
			Err:       ErrModelNotSet,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	table := r.service.Table(r.tableName(ctx))

	iter := table.Get(indexInput.PartitionKey, indexInput.PartitionKeyValue).
		Range(indexInput.SortKey, dynamo.Equal, indexInput.SortKeyValue).
		Index(indexInput.IndexName).
		Filter(filterQuery, filterArgs...).
		Iter()

	result := []eh.Entity{}
	entity := r.factoryFn()
	for iter.Next(entity) {
		result = append(result, entity)
		entity = r.factoryFn()
	}

	return result, nil
}

// Save implements the Save method of the eventhorizon.WriteRepo interface.
func (r *Repo) Save(ctx context.Context, entity eh.Entity) error {
	table := r.service.Table(r.tableName(ctx))

	if entity.EntityID() == uuid.Nil {
		return eh.RepoError{
			Err:       eh.ErrCouldNotSaveEntity,
			BaseErr:   eh.ErrMissingEntityID,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	if err := table.Put(entity).Run(); err != nil {
		return eh.RepoError{
			Err:       eh.ErrCouldNotSaveEntity,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// Remove implements the Remove method of the eventhorizon.WriteRepo interface.
func (r *Repo) Remove(ctx context.Context, id uuid.UUID) error {
	table := r.service.Table(r.tableName(ctx))

	if err := table.Delete("ID", id.String()).Run(); err != nil {
		return eh.RepoError{
			Err:       eh.ErrEntityNotFound,
			BaseErr:   err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// SetEntityFactory sets a factory function that creates concrete entity types.
func (r *Repo) SetEntityFactory(f func() eh.Entity) {
	r.factoryFn = f
}

// IndexInput is all the params we need to filter on an index
type IndexInput struct {
	IndexName         string
	PartitionKey      string
	PartitionKeyValue interface{}
	SortKey           string
	SortKeyValue      interface{}
}
