package database

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/registry/internal/model"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoDB is an implementation of the Database interface using MongoDB
type MongoDB struct {
	client             *mongo.Client
	database           *mongo.Database
	serverCollection   *mongo.Collection
	metadataCollection *mongo.Collection
}

// NewMongoDB creates a new instance of the MongoDB database
func NewMongoDB(ctx context.Context, connectionURI, databaseName, collectionName, metadataCollectionName string) (*MongoDB, error) {
	// Set default metadata collection name if not provided
	if metadataCollectionName == "" {
		metadataCollectionName = collectionName + "_metadata"
	}

	// Set client options and connect to MongoDB
	clientOptions := options.Client().ApplyURI(connectionURI)
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return nil, err
	}

	// Ping the MongoDB server to verify the connection
	if err = client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	// Get database and collection
	database := client.Database(databaseName)
	serverCollection := database.Collection(collectionName)
	metadataCollection := database.Collection(metadataCollectionName)

	// Create indexes for better query performance
	models := []mongo.IndexModel{
		{
			Keys: bson.D{bson.E{Key: "name", Value: 1}},
		},
		{
			Keys:    bson.D{bson.E{Key: "id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		// add an index for the combination of name and version
		{
			Keys:    bson.D{bson.E{Key: "name", Value: 1}, bson.E{Key: "version_detail.version", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
	}

	_, err = serverCollection.Indexes().CreateMany(ctx, models)
	if err != nil {
		// Mongo will error if the index already exists, we can ignore this and continue.
		var commandError mongo.CommandError
		if errors.As(err, &commandError) && commandError.Code != 86 {
			return nil, err
		}
		log.Printf("Indexes already exists, skipping.")
	}

	// Create indexes for metadata collection
	tokenIndexes := []mongo.IndexModel{
		{
			Keys:    bson.D{bson.E{Key: "server_id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
	}

	_, err = metadataCollection.Indexes().CreateMany(ctx, tokenIndexes)
	if err != nil {
		// Mongo will error if the index already exists, we can ignore this and continue.
		var commandError mongo.CommandError
		if errors.As(err, &commandError) && commandError.Code != 86 {
			return nil, err
		}
		log.Printf("Metadata collection indexes already exist, skipping.")
	}

	return &MongoDB{
		client:             client,
		database:           database,
		serverCollection:   serverCollection,
		metadataCollection: metadataCollection,
	}, nil
}

// List retrieves MCPRegistry entries with optional filtering and pagination
func (db *MongoDB) List(
	ctx context.Context,
	filter map[string]any,
	cursor string,
	limit int,
) ([]*model.Server, string, error) {
	if limit <= 0 {
		// Set default limit if not provided
		limit = 10
	}

	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}

	// Convert Go map to MongoDB filter
	mongoFilter := bson.M{
		"version_detail.is_latest": true,
	}
	// Map common filter keys to MongoDB document paths
	for k, v := range filter {
		// Handle nested fields with dot notation
		switch k {
		case "version":
			mongoFilter["version_detail.version"] = v
		case "name":
			mongoFilter["name"] = v
		default:
			mongoFilter[k] = v
		}
	}

	// Setup pagination options
	findOptions := options.Find()

	// If cursor is provided, add condition to filter to only get records after the cursor
	if cursor != "" {
		// Validate that the cursor is a valid UUID
		if _, err := uuid.Parse(cursor); err != nil {
			return nil, "", fmt.Errorf("invalid cursor format: %w", err)
		}

		// Fetch the document at the cursor to get its sort values
		var cursorDoc model.Server
		err := db.serverCollection.FindOne(ctx, bson.M{"id": cursor}).Decode(&cursorDoc)
		if err != nil {
			if !errors.Is(err, mongo.ErrNoDocuments) {
				return nil, "", err
			}
			// If cursor document not found, start from beginning
		} else {
			// Use the cursor document's ID to paginate (records with ID > cursor's ID)
			mongoFilter["id"] = bson.M{"$gt": cursor}
		}
	}

	// Set sort order by ID (for consistent pagination)
	findOptions.SetSort(bson.M{"id": 1})

	// Set limit if provided and valid
	if limit > 0 {
		findOptions.SetLimit(int64(limit))
	}

	// Execute find operation with options
	mongoCursor, err := db.serverCollection.Find(ctx, mongoFilter, findOptions)
	if err != nil {
		return nil, "", err
	}
	defer mongoCursor.Close(ctx)

	// Decode results
	var results []*model.Server
	if err = mongoCursor.All(ctx, &results); err != nil {
		return nil, "", err
	}

	// Determine the next cursor
	nextCursor := ""
	if len(results) > 0 && limit > 0 && len(results) >= limit {
		// Use the last item's ID as the next cursor
		nextCursor = results[len(results)-1].ID
	}

	return results, nextCursor, nil
}

// GetByID retrieves a single ServerDetail by its ID
func (db *MongoDB) GetByID(ctx context.Context, id string) (*model.ServerDetail, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Create a filter for the ID
	filter := bson.M{"id": id}

	// Find the entry in the database
	var entry model.ServerDetail
	err := db.serverCollection.FindOne(ctx, filter).Decode(&entry)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("error retrieving entry: %w", err)
	}

	// Create and return a ServerDetail from the entry data
	return &entry, nil
}

// Publish adds a new ServerDetail to the database
func (db *MongoDB) Publish(ctx context.Context, serverDetail *model.ServerDetail) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// find a server detail with the same name and check that the current version is greater than the existing one
	filter := bson.M{
		"name":                     serverDetail.Name,
		"version_detail.is_latest": true,
	}

	var existingEntry model.ServerDetail
	err := db.serverCollection.FindOne(ctx, filter).Decode(&existingEntry)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("error checking existing entry: %w", err)
	}

	// check that the current version is greater than the existing one
	if serverDetail.VersionDetail.Version <= existingEntry.VersionDetail.Version {
		return fmt.Errorf("version must be greater than existing version")
	}

	serverDetail.ID = uuid.New().String()
	serverDetail.VersionDetail.IsLatest = true
	serverDetail.VersionDetail.ReleaseDate = time.Now().Format(time.RFC3339)

	// Insert the entry into the database
	_, err = db.serverCollection.InsertOne(ctx, serverDetail)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("error inserting entry: %w", err)
	}

	// update the existing entry to not be the latest version
	if existingEntry.ID != "" {
		_, err = db.serverCollection.UpdateOne(
			ctx,
			bson.M{"id": existingEntry.ID},
			bson.M{"$set": bson.M{"version_detail.islatest": false}})
		if err != nil {
			return fmt.Errorf("error updating existing entry: %w", err)
		}
	}

	return nil
}

// ImportSeed imports initial data from a seed file into MongoDB
func (db *MongoDB) ImportSeed(ctx context.Context, seedFilePath string) error {
	// Read the seed file
	servers, err := ReadSeedFile(seedFilePath)
	if err != nil {
		return fmt.Errorf("failed to read seed file: %w", err)
	}

	collection := db.serverCollection

	log.Printf("Importing %d servers into collection %s", len(servers), collection.Name())

	for i, server := range servers {
		if server.ID == "" || server.Name == "" {
			log.Printf("Skipping server %d: ID or Name is empty", i+1)
			continue
		}

		if server.VersionDetail.Version == "" {
			server.VersionDetail.Version = "0.0.1-seed"
			server.VersionDetail.ReleaseDate = time.Now().Format(time.RFC3339)
			server.VersionDetail.IsLatest = true
		}

		// Create filter based on server ID
		filter := bson.M{"id": server.ID}

		// Create update document
		update := bson.M{"$set": server}

		// Use upsert to create if not exists or update if exists
		opts := options.Update().SetUpsert(true)
		result, err := collection.UpdateOne(ctx, filter, update, opts)
		if err != nil {
			log.Printf("Error importing server %s: %v", server.ID, err)
			continue
		}

		switch {
		case result.UpsertedCount > 0:
			log.Printf("[%d/%d] Created server: %s", i+1, len(servers), server.Name)
		case result.ModifiedCount > 0:
			log.Printf("[%d/%d] Updated server: %s", i+1, len(servers), server.Name)
		default:
			log.Printf("[%d/%d] Server already up to date: %s", i+1, len(servers), server.Name)
		}
	}

	log.Println("MongoDB database import completed successfully")
	return nil
}

// Close closes the database connection
func (db *MongoDB) Close() error {
	return db.client.Disconnect(context.Background())
}

// Connection returns information about the database connection
func (db *MongoDB) Connection() *ConnectionInfo {
	isConnected := false
	// Check if the client is connected
	if db.client != nil {
		// A quick ping with 1 second timeout to verify connection
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := db.client.Ping(ctx, nil)
		isConnected = (err == nil)
	}

	return &ConnectionInfo{
		Type:        ConnectionTypeMongoDB,
		IsConnected: isConnected,
		Raw:         db.client,
	}
}

// StoreVerificationToken stores a verification token for a server
func (db *MongoDB) StoreVerificationToken(ctx context.Context, serverID string, token *model.VerificationToken) error {
	metadata := &model.Metadata{
		ServerID:          serverID,
		VerificationToken: token,
	}

	_, err := db.metadataCollection.InsertOne(ctx, metadata)
	if err != nil {
		// Check if it's a duplicate key error
		var writeErr mongo.WriteException
		if errors.As(err, &writeErr) {
			for _, writeError := range writeErr.WriteErrors {
				if writeError.Code == 11000 { // Duplicate key error
					return ErrAlreadyExists
				}
			}
		}
		return fmt.Errorf("failed to store verification token: %w", err)
	}
	return nil
}

// GetVerificationToken retrieves a verification token by server ID
func (db *MongoDB) GetVerificationToken(ctx context.Context, serverID string) (*model.VerificationToken, error) {
	filter := bson.M{
		"server_id": serverID,
	}

	var metadata model.Metadata
	err := db.metadataCollection.FindOne(ctx, filter).Decode(&metadata)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get verification token: %w", err)
	}

	if metadata.VerificationToken == nil {
		return nil, fmt.Errorf("verification token data is missing from metadata")
	}

	return metadata.VerificationToken, nil
}

// GetVerifiedDomains retrieves all domains that are currently verified
func (db *MongoDB) GetVerifiedDomains(ctx context.Context) ([]string, error) {
	filter := bson.M{
		"domain_verification.status": model.VerificationStatusVerified,
	}
	
	cursor, err := db.metadataCollection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query verified domains: %w", err)
	}
	defer cursor.Close(ctx)
	
	var domains []string
	for cursor.Next(ctx) {
		var metadata model.Metadata
		if err := cursor.Decode(&metadata); err != nil {
			log.Printf("Failed to decode metadata: %v", err)
			continue
		}
		
		if metadata.DomainVerification != nil {
			domains = append(domains, metadata.DomainVerification.Domain)
		}
	}
	
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("cursor error: %w", err)
	}
	
	return domains, nil
}

// GetDomainVerification retrieves domain verification details
func (db *MongoDB) GetDomainVerification(ctx context.Context, domain string) (*model.DomainVerification, error) {
	filter := bson.M{
		"domain_verification.domain": domain,
	}
	
	var metadata model.Metadata
	err := db.metadataCollection.FindOne(ctx, filter).Decode(&metadata)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get domain verification: %w", err)
	}
	
	if metadata.DomainVerification == nil {
		return nil, ErrNotFound
	}
	
	return metadata.DomainVerification, nil
}

// UpdateDomainVerification updates or creates domain verification record
func (db *MongoDB) UpdateDomainVerification(ctx context.Context, domainVerification *model.DomainVerification) error {
	filter := bson.M{
		"domain_verification.domain": domainVerification.Domain,
	}
	
	update := bson.M{
		"$set": bson.M{
			"domain_verification": domainVerification,
		},
		"$setOnInsert": bson.M{
			"server_id": uuid.New().String(),
		},
	}
	
	opts := options.Update().SetUpsert(true)
	_, err := db.metadataCollection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("failed to update domain verification: %w", err)
	}
	
	return nil
}

// CleanupOldVerifications removes old verification records before the given time
func (db *MongoDB) CleanupOldVerifications(ctx context.Context, before time.Time) (int, error) {
	filter := bson.M{
		"domain_verification.status": model.VerificationStatusFailed,
		"domain_verification.last_verification_attempt": bson.M{
			"$lt": before,
		},
	}
	
	result, err := db.metadataCollection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old verifications: %w", err)
	}
	
	return int(result.DeletedCount), nil
}
