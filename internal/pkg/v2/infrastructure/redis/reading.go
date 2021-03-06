//
// Copyright (C) 2020 IOTech Ltd
//
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"encoding/json"
	"fmt"

	"github.com/edgexfoundry/edgex-go/internal/pkg/common"
	"github.com/edgexfoundry/go-mod-core-contracts/errors"
	"github.com/edgexfoundry/go-mod-core-contracts/v2"
	"github.com/edgexfoundry/go-mod-core-contracts/v2/models"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
)

const (
	ReadingsCollection           = "v2:reading"
	ReadingsCollectionCreated    = ReadingsCollection + ":" + v2.Created
	ReadingsCollectionDeviceName = ReadingsCollection + ":" + v2.Device + ":" + v2.Name
)

var emptyBinaryValue = make([]byte, 0)

// asyncDeleteReadingsByIds deletes all readings with given reading Ids.  This function is implemented to be run as a
// separate gorountine in the background to achieve better performance, so this function return nothing.  When
// encountering any errors during deletion, this function will simply log the error.
func (c *Client) asyncDeleteReadingsByIds(readingIds []string) {
	conn := c.Pool.Get()
	defer conn.Close()

	var readings [][]byte
	//start a transaction to get all readings
	readings, edgeXerr := getObjectsByIds(conn, common.ConvertStringsToInterfaces(readingIds))
	if edgeXerr != nil {
		c.loggingClient.Error(fmt.Sprintf("Deleted readings failed while retrieving objects by Ids.  Err: %s", edgeXerr.DebugMessages()))
		return
	}

	// iterate each readings for deletion in batch
	queriesInQueue := 0
	r := models.BaseReading{}
	_ = conn.Send(MULTI)
	for i, reading := range readings {
		err := json.Unmarshal(reading, &r)
		if err != nil {
			c.loggingClient.Error(fmt.Sprintf("unable to marshal reading.  Err: %s", err.Error()))
			continue
		}
		storedKey := readingStoredKey(r.Id)
		_ = conn.Send(UNLINK, storedKey)
		_ = conn.Send(ZREM, ReadingsCollection, storedKey)
		_ = conn.Send(ZREM, ReadingsCollectionCreated, storedKey)
		_ = conn.Send(ZREM, fmt.Sprintf("%s:%s", ReadingsCollectionDeviceName, r.DeviceName), storedKey)
		queriesInQueue++

		if queriesInQueue >= c.BatchSize {
			_, err = conn.Do(EXEC)
			if err != nil {
				c.loggingClient.Error(fmt.Sprintf("unable to execute batch reading deletion.  Err: %s", err.Error()))
				continue
			}
			// reset queriesInQueue to zero if EXEC is successfully executed without error
			queriesInQueue = 0
			// rerun another transaction when reading iteration is not finished
			if i < len(readings)-1 {
				_ = conn.Send(MULTI)
			}
		}
	}

	if queriesInQueue > 0 {
		_, err := conn.Do(EXEC)
		if err != nil {
			c.loggingClient.Error(fmt.Sprintf("unable to execute batch reading deletion.  Err: %s", err.Error()))
		}
	}
}

// readingStoredKey return the reading's stored key which combines the collection name and object id
func readingStoredKey(id string) string {
	return fmt.Sprintf("%s:%s", ReadingsCollection, id)
}

// Add a reading to the database
func addReading(conn redis.Conn, r models.Reading) (reading models.Reading, edgeXerr errors.EdgeX) {
	var m []byte
	var err error
	var baseReading *models.BaseReading
	switch newReading := r.(type) {
	case models.BinaryReading:
		// Clear the binary data since we do not want to persist binary data to save on memory.
		newReading.BinaryValue = emptyBinaryValue

		baseReading = &newReading.BaseReading
		if err = checkReadingValue(baseReading); err != nil {
			return nil, errors.NewCommonEdgeXWrapper(err)
		}
		m, err = json.Marshal(newReading)
		reading = newReading
	case models.SimpleReading:
		baseReading = &newReading.BaseReading
		if err = checkReadingValue(baseReading); err != nil {
			return nil, errors.NewCommonEdgeXWrapper(err)
		}
		m, err = json.Marshal(newReading)
		reading = newReading
	default:
		return nil, errors.NewCommonEdgeX(errors.KindContractInvalid, "unsupported reading type", nil)
	}

	if err != nil {
		return nil, errors.NewCommonEdgeX(errors.KindContractInvalid, "reading parsing failed", err)
	}
	storedKey := readingStoredKey(baseReading.Id)
	// use the SET command to save reading as blob
	_ = conn.Send(SET, storedKey, m)
	_ = conn.Send(ZADD, ReadingsCollection, 0, storedKey)
	_ = conn.Send(ZADD, ReadingsCollectionCreated, baseReading.Created, storedKey)
	_ = conn.Send(ZADD, fmt.Sprintf("%s:%s", ReadingsCollectionDeviceName, baseReading.DeviceName), baseReading.Created, storedKey)

	return reading, nil
}

// Remove a reading out of the database
func deleteReadingById(conn redis.Conn, id string) (edgeXerr errors.EdgeX) {
	r := models.BaseReading{}
	storedKey := readingStoredKey(id)
	edgeXerr = getObjectById(conn, storedKey, &r)
	if edgeXerr != nil {
		return edgeXerr
	}

	_ = conn.Send(MULTI)
	_ = conn.Send(UNLINK, storedKey)
	_ = conn.Send(ZREM, ReadingsCollection, storedKey)
	_ = conn.Send(ZREM, ReadingsCollectionCreated, storedKey)
	_ = conn.Send(ZREM, fmt.Sprintf("%s:%s", ReadingsCollectionDeviceName, r.DeviceName), storedKey)
	_, err := conn.Do(EXEC)
	if err != nil {
		return errors.NewCommonEdgeX(errors.KindDatabaseError, fmt.Sprintf("reading[id:%s] delete failed", id), err)
	}

	return nil
}

func checkReadingValue(b *models.BaseReading) errors.EdgeX {
	if b.Created == 0 {
		b.Created = common.MakeTimestamp()
	}
	// check if id is a valid uuid
	if b.Id == "" {
		b.Id = uuid.New().String()
	} else {
		_, err := uuid.Parse(b.Id)
		if err != nil {
			return errors.NewCommonEdgeX(errors.KindInvalidId, "uuid parsing failed", err)
		}
	}
	return nil
}

func readingsByEventId(conn redis.Conn, eventId string) (readings []models.Reading, edgeXerr errors.EdgeX) {
	objects, err := getObjectsByRange(conn, fmt.Sprintf("%s:%s", EventsCollectionReadings, eventId), 0, -1)
	if errors.Kind(err) == errors.KindEntityDoesNotExist {
		return // Empty Readings in an Event is not an error
	} else if err != nil {
		return readings, errors.NewCommonEdgeXWrapper(err)
	}

	readings = make([]models.Reading, len(objects))
	for i, in := range objects {
		sr := models.SimpleReading{}
		err := json.Unmarshal(in, &sr)
		if err != nil {
			return []models.Reading{}, errors.NewCommonEdgeX(errors.KindDatabaseError, "reading format parsing failed from the database", err)
		}
		readings[i] = sr
	}

	return
}
