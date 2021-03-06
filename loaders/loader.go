/*
Real-time Online/Offline Charging System (OCS) for Telecom & ISP environments
Copyright (C) ITsysCOM GmbH

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package loaders

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/cgrates/cgrates/config"
	"github.com/cgrates/cgrates/engine"
	"github.com/cgrates/cgrates/utils"
)

type openedCSVFile struct {
	fileName string
	rdr      io.ReadCloser // keep reference so we can close it when done
	csvRdr   *csv.Reader
}

// Loader is one instance loading from a folder
type Loader struct {
	ldrID         string
	tpInDir       string
	tpOutDir      string
	lockFilename  string
	cacheSConns   []*config.HaPoolConfig
	fieldSep      string
	dataTpls      map[string][]*config.CfgCdrField     // map[loaderType]*config.CfgCdrField
	rdrs          map[string]map[string]*openedCSVFile // map[loaderType]map[fileName]*openedCSVFile for common incremental read
	procRows      int                                  // keep here the last processed row in the file/-s
	bufLoaderData map[string][]LoaderData              // cache of data read, indexed on tenantID
	dm            *engine.DataManager
	timezone      string
}

// ProcessFolder will process the content in the folder with locking
func (ldr *Loader) ProcessFolder() (err error) {
	if err = ldr.lockFolder(); err != nil {
		return
	}
	defer ldr.unlockFolder()
	for ldrType := range ldr.rdrs {
		if err = ldr.openFiles(ldrType); err != nil {
			utils.Logger.Warning(fmt.Sprintf("<%s> loaderType: <%s> cannot open files, err: %s",
				utils.LoaderS, ldrType, err.Error()))
			continue
		}
		if err = ldr.processContent(ldrType); err != nil {
			utils.Logger.Warning(fmt.Sprintf("<%s> loaderType: <%s>, err: %s",
				utils.LoaderS, ldrType, err.Error()))
		}
	}
	return
}

// lockFolder will attempt to lock the folder by creating the lock file
func (ldr *Loader) lockFolder() (err error) {
	_, err = os.OpenFile(path.Join(ldr.tpInDir, ldr.lockFilename),
		os.O_RDONLY|os.O_CREATE, 0644)
	return
}

func (ldr *Loader) unlockFolder() (err error) {
	return os.Remove(path.Join(ldr.tpInDir,
		ldr.lockFilename))
}

// unreferenceFile will cleanup an used file by closing and removing from referece map
func (ldr *Loader) unreferenceFile(loaderType, fileName string) (err error) {
	openedCSVFile := ldr.rdrs[loaderType][fileName]
	ldr.rdrs[loaderType][fileName] = nil
	return openedCSVFile.rdr.Close()
}

func (ldr *Loader) storeLoadedData(loaderType string,
	lds map[string][]LoaderData) (err error) {
	switch loaderType {
	case utils.MetaAttributes:
		for _, lDataSet := range lds {
			attrModels := make(engine.TPAttributes, len(lDataSet))
			for i, ld := range lDataSet {
				attrModels[i] = new(engine.TPAttribute)
				if err = utils.UpdateStructWithIfaceMap(attrModels[i], ld); err != nil {
					return
				}
			}
			for _, tpApf := range attrModels.AsTPAttributes() {
				if apf, err := engine.APItoAttributeProfile(tpApf, ldr.timezone); err != nil {
					return err
				} else if err := ldr.dm.SetAttributeProfile(apf, true); err != nil {
					return err
				}
			}
		}
	}
	return
}

func (ldr *Loader) openFiles(loaderType string) (err error) {
	for fName := range ldr.rdrs[loaderType] {
		var rdr *os.File
		if rdr, err = os.Open(path.Join(ldr.tpInDir, fName)); err != nil {
			return err
		}
		ldr.rdrs[loaderType][fName] = &openedCSVFile{
			fileName: fName, rdr: rdr, csvRdr: csv.NewReader(rdr)}
		defer ldr.unreferenceFile(loaderType, fName)
	}
	return
}

func (ldr *Loader) processContent(loaderType string) (err error) {
	// start processing lines
	keepLooping := true // controls looping
	lineNr := 0
	for keepLooping {
		lineNr += 1
		var hasErrors bool
		lData := make(LoaderData) // one row
		for fName, rdr := range ldr.rdrs[loaderType] {
			var record []string
			if record, err = rdr.csvRdr.Read(); err != nil {
				if err == io.EOF {
					keepLooping = false
					break
				}
				hasErrors = true
				utils.Logger.Warning(
					fmt.Sprintf("<%s> <%s> reading line: %d, error: %s",
						utils.LoaderS, ldr.ldrID, lineNr, err.Error()))
			}
			if hasErrors { // if any of the readers will give errors, we ignore the line
				continue
			}
			if err := lData.UpdateFromCSV(fName, record,
				ldr.dataTpls[utils.MetaAttributes]); err != nil {
				fmt.Sprintf("<%s> <%s> line: %d, error: %s",
					utils.LoaderS, ldr.ldrID, lineNr, err.Error())
				hasErrors = true
				continue
			}
			// Record from map
			// update dataDB
		}
		if len(lData) == 0 { // no data, could be the last line in file
			continue
		}
		tntID := lData.TenantID()
		if _, has := ldr.bufLoaderData[tntID]; !has &&
			len(ldr.bufLoaderData) == 1 { // process previous records before going futher
			var prevTntID string
			for prevTntID = range ldr.bufLoaderData {
				break // have stolen the existing key in buffer
			}
			if err = ldr.storeLoadedData(loaderType,
				map[string][]LoaderData{prevTntID: ldr.bufLoaderData[prevTntID]}); err != nil {
				return
			}
			delete(ldr.bufLoaderData, prevTntID)
		}
		ldr.bufLoaderData[tntID] = append(ldr.bufLoaderData[tntID], lData)
	}
	// proceed with last element in bufLoaderData
	var tntID string
	for tntID = range ldr.bufLoaderData {
		break // get the first tenantID
	}
	if err = ldr.storeLoadedData(loaderType,
		map[string][]LoaderData{tntID: ldr.bufLoaderData[tntID]}); err != nil {
		return
	}
	delete(ldr.bufLoaderData, tntID)
	return
}
