package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/j4/gosm"
	"github.com/jmoiron/sqlx"
	"log"
	"os"
	"runtime"
	"strconv"
	"time"
	"ttnmapper-heatmap-tile-generator/types"
)

type Configuration struct {
	DirGlobalHeatmap  string
	DirGatewayHeatmap string
	DirFogOfWar       string
	DirGatewayCount   string

	MysqlHost     string
	MysqlPort     string
	MysqlUser     string
	MysqlPassword string
	MysqlDatabase string

	PromethuesPort string

	SleepDuration     string // time to sleep between database calls when there was nothing found to process
	LastQueuedWindow  int
	MinZoomLevel      int
	MaxZoomLevel      int
	FileWriterThreads int
}

var myConfiguration = Configuration{
	DirGlobalHeatmap:  "./heatmapTiles",
	DirGatewayHeatmap: "./gwTiles",
	DirFogOfWar:       "./fowTiles",
	DirGatewayCount:   "./gwCountTiles",

	MysqlHost:     "localhost",
	MysqlPort:     "3306",
	MysqlUser:     "user",
	MysqlPassword: "password",
	MysqlDatabase: "database",

	PromethuesPort: "2114",

	SleepDuration:     "10s",
	LastQueuedWindow:  1,
	MinZoomLevel:      1,
	MaxZoomLevel:      19,
	FileWriterThreads: 4,
}

func main() {

	// Read configs from file
	file, err := os.Open("conf.json")
	if err != nil {
		log.Print(err.Error())
	}
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&myConfiguration)
	if err != nil {
		log.Print(err.Error())
	}
	err = file.Close()
	if err != nil {
		log.Print(err.Error())
	}
	log.Printf("Using configuration: %+v", myConfiguration) // output: [UserA, UserB]

	// Start threads to handle slow PNG encoding and file writing
	for i := 0; i < myConfiguration.FileWriterThreads; i++ {
		go listenForFilesToWrite()
	}

	// Set up db connection
	db, err := sqlx.Open("mysql", myConfiguration.MysqlUser+":"+myConfiguration.MysqlPassword+"@tcp("+myConfiguration.MysqlHost+":"+myConfiguration.MysqlPort+")/"+myConfiguration.MysqlDatabase+"?parseTime=true")
	if err != nil {
		panic(err.Error())
	}
	err = db.Ping()
	if err != nil {
		panic(err.Error())
	}
	defer db.Close()

	//stmtSelectToProcess, err := db.PrepareNamed("SELECT * FROM tiles_to_redraw WHERE 1 ORDER BY last_queued ASC LIMIT 1")
	//if err != nil {
	//	panic(err.Error())
	//}
	//defer stmtSelectToProcess.Close()

	stmtSelectAggData, err := db.PrepareNamed("SELECT * FROM agg_zoom_19 WHERE x >= :xNw AND y >= :yNw AND x < :xSe AND y < :ySe")
	if err != nil {
		panic(err.Error())
	}
	defer stmtSelectAggData.Close()

	//stmtDeleteToProcess, err := db.PrepareNamed("DELETE FROM tiles_to_redraw WHERE id = :id")
	//if err != nil {
	//	panic(err.Error())
	//}
	//defer stmtDeleteToProcess.Close()

	sleepDuration, err := time.ParseDuration(myConfiguration.SleepDuration)
	if err != nil {
		log.Fatal(err.Error())
	}

	for {

		tileStart := time.Now()

		tilesToReprocess := []types.MysqlTileToRedraw{}
		// Give a one minute buffer time
		err = db.Select(&tilesToReprocess,
			"SELECT * FROM tiles_to_redraw WHERE `last_queued` < (NOW() - INTERVAL ? SECOND) AND z>=? AND z<=? ORDER BY last_queued ASC LIMIT 1",
			myConfiguration.LastQueuedWindow,
			myConfiguration.MinZoomLevel,
			myConfiguration.MaxZoomLevel)
		if err != nil {
			fmt.Println(err)
			return
		}

		if len(tilesToReprocess) == 0 {
			// Nothing found. Sleep a bit and try again.
			time.Sleep(sleepDuration)
			continue
		}

		tileToProcess := tilesToReprocess[0]
		x := tileToProcess.X
		y := tileToProcess.Y
		z := tileToProcess.Z

		// Remove processed tiles from the queue
		//result, err := stmtDeleteToProcess.Exec(tileToProcess)
		//if err != nil {
		//	log.Printf(err.Error())
		//}

		// Because we redraw the bordering tiles too, remove them from the db if they are queued
		result, err := db.Exec("DELETE FROM tiles_to_redraw WHERE x>=? AND x<=? AND y>=? AND y<=? AND z=?", x-1, x+1, y-1, y+1, z)
		if err != nil {
			log.Printf(err.Error())
		}

		rowsAffected, err := result.RowsAffected()
		log.Printf("Deleted %d rows", rowsAffected)

		// Testing tiles
		//https://ttnmapper.org/tms/index.php?tile=15/18101/19671
		//https://ttnmapper.org/tms/index.php?tile=18/144812/157369
		// Road offset example: http://dev.ttnmapper.org/tms/fog_of_war/12/2104/1350.png

		//x = 2104
		//y = 1350
		//z = 12

		//divisionFactor := 3
		//x /= int(math.Pow(2, float64(divisionFactor)))
		//y /= int(math.Pow(2, float64(divisionFactor)))
		//z -= divisionFactor

		log.Printf("Generating tile for %d/%d/%d", z, x, y)

		// Select data for 25 tiles. We draw 9 tiles and have one tile as padding for boundary cases.
		tileNWpad := gosm.NewTileWithXY(x-2, y-2, z)
		tileSEpad := gosm.NewTileWithXY(x+3, y+3, z)

		// For the tileNW we need to reprocess, find all z-19 tiles that falls inside it
		tileNW19 := gosm.NewTileWithLatLong(tileNWpad.Lat, tileNWpad.Long, 19)
		tileSE19 := gosm.NewTileWithLatLong(tileSEpad.Lat, tileSEpad.Long, 19)

		// Query
		aggCoords := map[string]interface{}{"xNw": tileNW19.X, "yNw": tileNW19.Y, "xSe": tileSE19.X, "ySe": tileSE19.Y}

		var entries = []types.MysqlAggGridcell{}
		start := time.Now()
		rows, err := stmtSelectAggData.Queryx(aggCoords)
		if err != nil {
			log.Printf(err.Error())
		}
		for rows.Next() {
			var entry = types.MysqlAggGridcell{}
			err = rows.StructScan(&entry)
			if err != nil {
				log.Print(err.Error())
			}
			entries = append(entries, entry)
		}
		elapsed := time.Since(start)
		log.Printf("  DB select took %s", elapsed)

		log.Printf(" using %d points", len(entries))

		start = time.Now()
		drawGlobalTile(x, y, z, entries)
		elapsed = time.Since(start)
		log.Printf("  Global tile took %s", elapsed)

		//start = time.Now()
		//drawPerGatewayTiles(x, y, z, entries)
		//elapsed = time.Since(start)
		//log.Printf("  Gateways tiles took %s", elapsed)

		start = time.Now()
		drawGatewayCountTile(x, y, z, entries)
		elapsed = time.Since(start)
		log.Printf("  Gateway count tile took %s", elapsed)

		start = time.Now()
		drawFogOfWarTile(x, y, z, entries)
		elapsed = time.Since(start)
		log.Printf("  FOW tile took %s", elapsed)

		elapsed = time.Since(tileStart)
		log.Printf("Tile took %s", elapsed)
	}
}

func CreateDirIfNotExist(dir string) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			panic(err)
		}
	}
}

func getGID() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}
