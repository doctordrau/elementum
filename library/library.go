package library

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/missinggo/perf"
	"github.com/asdine/storm"
	"github.com/asdine/storm/q"
	"github.com/op/go-logging"

	"github.com/elgatito/elementum/cache"
	"github.com/elgatito/elementum/config"
	"github.com/elgatito/elementum/database"
	"github.com/elgatito/elementum/library/uid"
	"github.com/elgatito/elementum/tmdb"
	"github.com/elgatito/elementum/trakt"
	"github.com/elgatito/elementum/util"
	"github.com/elgatito/elementum/xbmc"
)

const (
	movieType   = "movie"
	showType    = "show"
	episodeType = "episode"

	trueType  = "true"
	falseType = "false"
)

const (
	// MovieType ...
	MovieType = iota
	// ShowType ...
	ShowType
	// SeasonType ...
	SeasonType
	// EpisodeType ...
	EpisodeType
)

const (
	// StateDeleted ...
	StateDeleted = iota
	// StateActive ...
	StateActive
)

const (
	// ActionUpdate ...
	ActionUpdate = iota
	// ActionDelete ...
	ActionDelete
	// ActionSafeDelete ...
	ActionSafeDelete
)

const (
	// TVDBScraper ...
	TVDBScraper = iota
	// TMDBScraper ...
	TMDBScraper
	// TraktScraper ...
	TraktScraper
	// IMDBScraper ...
	IMDBScraper
)

const (
	// Active ...
	Active = iota
	// Deleted ...
	Deleted
)
const (
	// Delete ...
	Delete = iota
	// Update ...
	Update
	// Batch ...
	Batch
	// BatchDelete ...
	BatchDelete
	// DeleteTorrent ...
	DeleteTorrent
)

var (
	removedEpisodes = make(chan *removedEpisode)
	closer          = util.Event{}

	log = logging.MustGetLogger("library")

	cacheStore *cache.DBStore

	initialized = false

	resolveRegexp = regexp.MustCompile(`^plugin://plugin.video.elementum.*?(\d+)(\W|$)`)

	pendingShows = map[int]bool{}

	lock = sync.Mutex{}

	ErrVideoRemoved = errors.New("Video is marked as removed")
)

// InitDB ...
func InitDB() {
	cacheStore = cache.NewDBStore()
}

// Init makes preparations on program start
func Init() {
	InitDB()

	if err := checkMoviesPath(); err != nil {
		xbmc.Notify("Elementum", err.Error(), config.AddonIcon())
		return
	}
	if err := checkShowsPath(); err != nil {
		xbmc.Notify("Elementum", err.Error(), config.AddonIcon())
		return
	}

	go func() {
		// Give time to Kodi to start its JSON-RPC service
		time.Sleep(5 * time.Second)

		// After re-configure check Trakt authorization
		if config.Get().TraktToken != "" && !config.Get().TraktAuthorized {
			trakt.GetLastActivities()
		}

		RefreshLocal()
		Refresh()
		initialized = true
	}()

	// Removed episodes debouncer
	go func() {
		var episodes []*removedEpisode

		closing := closer.C()
		timer := time.NewTicker(3 * time.Second)
		defer timer.Stop()
		defer close(removedEpisodes)

		for {
			select {
			case <-closing:
				return

			case <-timer.C:
				if len(episodes) == 0 {
					break
				}

				shows := make(map[string][]*removedEpisode, 0)
				for _, episode := range episodes {
					shows[episode.ShowName] = append(shows[episode.ShowName], episode)
				}

				var label string
				var labels []string
				if len(episodes) > 1 {
					for showName, showEpisodes := range shows {
						var libraryTotal int
						if !uid.HasShows() {
							break
						}
						show, err := uid.GetShowByTMDB(showEpisodes[0].ShowID)
						if show != nil && err == nil {
							libraryTotal = len(show.Episodes)
						}
						if libraryTotal == 0 {
							break
						}
						if len(showEpisodes) == libraryTotal {
							ID := strconv.Itoa(showEpisodes[0].ShowID)
							if _, _, err := RemoveShow(ID); err != nil {
								log.Error("Unable to remove show after removing all episodes...")
							}
						} else {
							labels = append(labels, fmt.Sprintf("%d episodes of %s", len(showEpisodes), showName))
						}

						// Add single episodes to removed prefix
						var tmdbIDs []int
						for _, showEpisode := range showEpisodes {
							tmdbIDs = append(tmdbIDs, showEpisode.ID)
						}
						if err := updateBatchDBItem(tmdbIDs, StateDeleted, EpisodeType, showEpisodes[0].ShowID); err != nil {
							log.Error(err)
						}
					}
					if len(labels) > 0 {
						label = strings.Join(labels, ", ")
						if xbmc.DialogConfirmFocused("Elementum", fmt.Sprintf("LOCALIZE[30278];;%s", label)) {
							xbmc.VideoLibraryClean()
						}
					}
				} else {
					for showName, episode := range shows {
						label = fmt.Sprintf("%s S%02dE%02d", showName, episode[0].Season, episode[0].Episode)
						if err := updateDBItem(episode[0].ID, StateDeleted, EpisodeType, episode[0].ShowID); err != nil {
							log.Error(err)
						}
					}
					if xbmc.DialogConfirmFocused("Elementum", fmt.Sprintf("LOCALIZE[30278];;%s", label)) {
						xbmc.VideoLibraryClean()
					}
				}

				episodes = make([]*removedEpisode, 0)

			case episode, ok := <-removedEpisodes:
				if !ok {
					break
				}
				episodes = append(episodes, episode)
			}
		}
	}()

	updateDelay := config.Get().UpdateDelay
	if updateDelay > 0 {
		if updateDelay < 10 {
			// Give time to Elementum to update its cache of libraryMovies, libraryShows and libraryEpisodes
			updateDelay = 10
		}
		go func() {
			time.Sleep(time.Duration(updateDelay) * time.Second)
			closing := closer.C()

			select {
			case <-closing:
				return
			default:
				PlanTraktUpdate()
				updateLibraryShows()
			}
		}()
	}

	log.Notice("Warming up caches...")
	go func() {
		time.Sleep(30 * time.Second)
		if !tmdb.WarmingUp.IsSet() {
			xbmc.Notify("Elementum", "LOCALIZE[30147]", config.AddonIcon())
		}
	}()

	started := time.Now()
	language := config.Get().Language
	tmdb.PopularMovies(tmdb.DiscoverFilters{}, language, 1)
	tmdb.PopularShows(tmdb.DiscoverFilters{}, language, 1)
	if _, _, err := trakt.TopMovies("trending", "1"); err != nil {
		log.Warning(err)
	}
	if _, _, err := trakt.TopShows("trending", "1"); err != nil {
		log.Warning(err)
	}

	tmdb.WarmingUp.Set()
	took := time.Since(started)
	if took.Seconds() > 30 {
		xbmc.Notify("Elementum", "LOCALIZE[30148]", config.AddonIcon())
	}
	log.Noticef("Caches warmed up in %s", took)

	updateFrequency := util.Max(1, config.Get().UpdateFrequency)
	traktFrequency := util.Max(1, config.Get().TraktSyncFrequencyMin)

	updateTicker := time.NewTicker(time.Duration(updateFrequency) * time.Hour)
	traktSyncTicker := time.NewTicker(time.Duration(traktFrequency) * time.Minute)
	markedForRemovalTicker := time.NewTicker(30 * time.Second)
	watcherTicker := time.NewTicker(1 * time.Second)

	defer updateTicker.Stop()
	defer traktSyncTicker.Stop()
	defer markedForRemovalTicker.Stop()
	defer watcherTicker.Stop()

	closing := closer.C()

	l := uid.Get()
	for {
		select {
		case <-watcherTicker.C:
			if !initialized || l.Running.IsOverall || l.Running.IsMovies || l.Running.IsShows || l.Running.IsEpisodes || l.Running.IsKodi || l.Running.IsTrakt {
				continue
			} else if l.Pending.IsKodi {
				go RefreshKodi()
			} else if l.Pending.IsTrakt {
				go RefreshTrakt()
			} else if l.Pending.IsMovies {
				go RefreshMovies()
			} else if l.Pending.IsShows {
				go RefreshShows()
			} else if l.Pending.IsEpisodes {
				go RefreshEpisodes()
			} else if l.Pending.IsOverall {
				go Refresh()
			}
		case <-updateTicker.C:
			if config.Get().UpdateFrequency > 0 && config.Get().LibraryEnabled && config.Get().LibrarySyncEnabled && (config.Get().LibrarySyncPlaybackEnabled || !xbmc.PlayerIsPlaying()) {
				go func() {
					if err := updateLibraryShows(); err != nil {
						log.Warning(err)
						return
					}
					PlanKodiUpdate()
				}()
			}
		case <-traktSyncTicker.C:
			PlanTraktUpdate()
		case <-markedForRemovalTicker.C:
			var items []database.BTItem
			database.GetStormDB().Select(q.Eq("State", database.StateDeleted)).Find(&items)

			for _, item := range items {
				// Remove from Elementum's library to prevent duplicates
				if item.Type == movieType {
					if uid.IsDuplicateMovie(strconv.Itoa(item.ID)) {
						if _, _, err := RemoveMovie(item.ID); err != nil {
							log.Warning("Nothing left to remove from Elementum")
						}
					}
				} else {
					if uid.IsDuplicateEpisode(item.ShowID, item.Season, item.Episode) {
						if err := RemoveEpisode(item.ID, item.ShowID, item.Season, item.Episode); err != nil {
							log.Warning(err)
						}
					}
				}

				database.GetStormDB().DeleteStruct(&item)
				log.Infof("Removed %s from database", item.InfoHash)
			}

		case <-closing:
			return
		}
	}
}

// MoviesLibraryPath contains calculated path for saving Movies strm files
func MoviesLibraryPath() string {
	return filepath.Join(config.Get().LibraryPath, "Movies")
}

// ShowsLibraryPath contains calculated path for saving Shows strm files
func ShowsLibraryPath() string {
	return filepath.Join(config.Get().LibraryPath, "Shows")
}

//
// Library updates
//
func updateLibraryShows() error {
	if !config.Get().LibraryEnabled || !config.Get().LibrarySyncEnabled || (!config.Get().LibrarySyncPlaybackEnabled && xbmc.PlayerIsPlaying()) {
		return nil
	}

	if err := checkShowsPath(); err != nil {
		return err
	}

	begin := time.Now()

	var lis []database.LibraryItem
	if err := database.GetStormDB().Select(q.Eq("MediaType", ShowType), q.Eq("State", StateActive)).Find(&lis); err != nil && err != storm.ErrNotFound {
		log.Infof("Could not get list of library items: %s", err)
	}

	for _, i := range lis {
		if closer.IsSet() {
			return nil
		}
		if i.ID == 0 || i.ShowID == 0 {
			continue
		}

		if _, err := writeShowStrm(i.ShowID, false, false); err != nil {
			log.Errorf("Error updating show: %s", err)
		}
	}

	log.Infof("Library updated in %s", time.Since(begin))
	PlanKodiUpdate()
	return nil
}

//
// Path checks
//
func checkLibraryPath() error {
	libraryPath := config.Get().LibraryPath
	if libraryPath == "" || libraryPath == "." {
		log.Warningf("Library path is not initialized")
		return errors.New("LOCALIZE[30220]")
	}
	if fileInfo, err := os.Stat(libraryPath); err != nil {
		if fileInfo == nil {
			log.Warningf("Library path is invalid")
			return errors.New("Invalid library path")
		}
		if !fileInfo.IsDir() {
			log.Warningf("Library path is not a directory")
			return errors.New("Library path is not a directory")
		}

		log.Warningf("Error getting Library path: %v", err)
		return err
	}
	return nil
}

func checkMoviesPath() error {
	if err := checkLibraryPath(); err != nil {
		return err
	}

	moviesLibraryPath := MoviesLibraryPath()
	if _, err := os.Stat(moviesLibraryPath); os.IsNotExist(err) {
		if err := os.Mkdir(moviesLibraryPath, 0755); err != nil {
			log.Error(err)
			return err
		}
	}
	return nil
}

func checkShowsPath() error {
	if err := checkLibraryPath(); err != nil {
		return err
	}

	showsLibraryPath := ShowsLibraryPath()
	if _, err := os.Stat(showsLibraryPath); os.IsNotExist(err) {
		if err := os.Mkdir(showsLibraryPath, 0755); err != nil {
			log.Error(err)
			return err
		}
	}
	return nil
}

//
// Writers
//

func writeMovieStrm(tmdbID string, force bool) (*tmdb.Movie, error) {
	// We should not write strm files for movies that are marked as deleted
	ID, _ := strconv.Atoi(tmdbID)
	if wasRemoved(ID, MovieType) && !force {
		return nil, ErrVideoRemoved
	}

	movie := tmdb.GetMovieByID(tmdbID, config.Get().StrmLanguage)
	if movie == nil {
		return nil, errors.New("Can't find the movie")
	}

	movieName := movie.OriginalTitle
	if config.Get().StrmLanguage != config.Get().Language && movie.Title != "" {
		movieName = movie.Title
	}
	movieStrm := util.ToFileName(fmt.Sprintf("%s (%s)", movieName, strings.Split(movie.ReleaseDate, "-")[0]))
	moviePath := filepath.Join(MoviesLibraryPath(), movieStrm)

	if _, err := os.Stat(moviePath); os.IsNotExist(err) {
		if err := os.Mkdir(moviePath, 0755); err != nil {
			log.Error(err)
			return movie, err
		}
	} else if force {
		os.Chtimes(moviePath, time.Now().Local(), time.Now().Local())
	}

	movieStrmPath := filepath.Join(moviePath, fmt.Sprintf("%s.strm", movieStrm))
	if config.Get().LibraryNFOMovies {
		writeMovieNFO(movie, filepath.Join(moviePath, fmt.Sprintf("%s.nfo", movieStrm)))
	}

	playLink := URLForXBMC("/library/movie/play/%s", tmdbID)
	if _, err := os.Stat(movieStrmPath); !force && err == nil {
		// log.Debugf("Movie strm file already exists at %s", movieStrmPath)
		// return movie, fmt.Errorf("LOCALIZE[30287];;%s", movie.Title)
		return movie, nil
	}
	if err := ioutil.WriteFile(movieStrmPath, []byte(playLink), 0644); err != nil {
		log.Errorf("Could not write strm file: %s", err)
		return movie, err
	}

	return movie, nil
}

func writeMovieNFO(m *tmdb.Movie, p string) error {
	out := `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>
<movie>
	<uniqueid type="unknown" default="false">%v</uniqueid>
	<uniqueid type="elementum" default="false">%v</uniqueid>
	<uniqueid type="tmdb" default="true">%v</uniqueid>
	<uniqueid type="imdb" default="false">%v</uniqueid>
	<uniqueid type="tvdb" default="false">%v</uniqueid>
</movie>
https://www.themoviedb.org/movie/%v
`
	out = fmt.Sprintf(out,
		m.ID,
		m.ID,
		m.ID,
		m.ExternalIDs.IMDBId,
		m.ExternalIDs.TVDBID,
		m.ID,
	)

	if m.ExternalIDs.IMDBId != "" {
		out += fmt.Sprintf("https://www.imdb.com/title/%s/\n", m.ExternalIDs.IMDBId)
	}

	if err := ioutil.WriteFile(p, []byte(out), 0644); err != nil {
		log.Errorf("Could not write NFO file: %s", err)
		return err
	}

	return nil
}

func writeShowStrm(showID int, adding, force bool) (*tmdb.Show, error) {
	// We should not write strm files for shows that are marked as deleted
	if wasRemoved(showID, ShowType) && !force {
		return nil, ErrVideoRemoved
	}

	defer perf.ScopeTimer()()

	show := tmdb.GetShow(showID, config.Get().StrmLanguage)
	if show == nil {
		return nil, fmt.Errorf("Unable to get show (%d)", showID)
	}

	showPath, showStrm := getShowPath(show)

	if _, err := os.Stat(showPath); os.IsNotExist(err) {
		if err := os.Mkdir(showPath, 0755); err != nil {
			log.Error(err)
			return show, err
		}
	} else if force {
		os.Chtimes(showPath, time.Now().Local(), time.Now().Local())
	}

	if config.Get().LibraryNFOShows {
		writeShowNFO(show, filepath.Join(showPath, "tvshow.nfo"))
	}

	addSpecials := config.Get().AddSpecials

	for _, season := range show.Seasons {
		if season.EpisodeCount == 0 {
			continue
		}
		if config.Get().ShowUnairedSeasons == false {
			if _, isExpired := util.AirDateWithExpireCheck(show.FirstAirDate, config.Get().ShowEpisodesOnReleaseDay); isExpired {
				continue
			}
		}
		if addSpecials == false && season.Season == 0 {
			continue
		}

		seasonTMDB := tmdb.GetSeason(showID, season.Season, config.Get().Language, len(show.Seasons))
		if seasonTMDB == nil {
			continue
		}
		episodes := seasonTMDB.Episodes

		var reAddIDs []int
		for _, episode := range episodes {
			if episode == nil {
				continue
			}

			if config.Get().ShowUnairedEpisodes == false {
				if episode.AirDate == "" {
					continue
				}
				if _, isExpired := util.AirDateWithExpireCheck(episode.AirDate, config.Get().ShowEpisodesOnReleaseDay); isExpired {
					continue
				}
			}

			if adding {
				reAddIDs = append(reAddIDs, episode.ID)
			}

			if !force && uid.IsDuplicateEpisode(showID, season.Season, episode.EpisodeNumber) {
				continue
			}

			episodeStrmPath := filepath.Join(showPath, fmt.Sprintf("%s S%02dE%02d.strm", showStrm, season.Season, episode.EpisodeNumber))
			playLink := URLForXBMC("/library/show/play/%d/%d/%d", showID, season.Season, episode.EpisodeNumber)
			if _, err := os.Stat(episodeStrmPath); !force && err == nil {
				continue
			}

			if err := ioutil.WriteFile(episodeStrmPath, []byte(playLink), 0644); err != nil {
				log.Error(err)
				return show, err
			}
		}
		if len(reAddIDs) > 0 {
			if err := updateBatchDBItem(reAddIDs, StateActive, EpisodeType, showID); err != nil {
				log.Error(err)
			}
		}
	}

	return show, nil
}

func writeShowNFO(s *tmdb.Show, p string) error {
	out := `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>
<tvshow>
	<uniqueid type="unknown" default="false">%v</uniqueid>
	<uniqueid type="elementum" default="false">%v</uniqueid>
	<uniqueid type="tmdb" default="true">%v</uniqueid>
	<uniqueid type="imdb" default="false">%v</uniqueid>
	<uniqueid type="tvdb" default="false">%v</uniqueid>
</tvshow>
https://www.themoviedb.org/tv/%v
`
	out = fmt.Sprintf(out,
		s.ID,
		s.ID,
		s.ID,
		s.ExternalIDs.IMDBId,
		s.ExternalIDs.TVDBID,
		s.ID,
	)

	if s.ExternalIDs.IMDBId != "" {
		out += fmt.Sprintf("https://www.imdb.com/title/%v/\n", s.ExternalIDs.IMDBId)
	}
	if s.ExternalIDs.TVDBID != "" {
		out += fmt.Sprintf("https://www.thetvdb.com/?tab=series&id=%v&lid=7\n", s.ExternalIDs.TVDBID)
	}

	if err := ioutil.WriteFile(p, []byte(out), 0644); err != nil {
		log.Errorf("Could not write NFO file: %s", err)
		return err
	}

	return nil
}

//
// Removers
//

// RemoveMovie removes movie from the library
func RemoveMovie(tmdbID int) (*tmdb.Movie, []string, error) {
	if err := checkMoviesPath(); err != nil {
		return nil, nil, err
	}
	defer func() {
		deleteDBItem(tmdbID, MovieType, true)
	}()

	ID := strconv.Itoa(tmdbID)
	movie := tmdb.GetMovieByID(ID, config.Get().StrmLanguage)
	if movie == nil {
		return nil, nil, errors.New("Can't resolve movie")
	}

	paths := getMoviePaths(movie)

	if len(paths) == 0 {
		log.Warningf("Cannot find directories with strm files")
		return movie, nil, errors.New("LOCALIZE[30282]")
	}
	ret := []string{}
	for path := range paths {
		if err := os.RemoveAll(path); err != nil {
			log.Error(err)
			return movie, nil, err
		}

		ret = append(ret, path)
		log.Warningf("Directory %s removed from disk", path)
	}

	log.Warningf("%s removed from library", movie.Title)
	return movie, ret, nil
}

// RemoveShow removes show from the library
func RemoveShow(tmdbID string) (*tmdb.Show, []string, error) {
	if err := checkShowsPath(); err != nil {
		return nil, nil, err
	}
	ID, _ := strconv.Atoi(tmdbID)
	defer func() {
		deleteDBItem(ID, ShowType, true)
	}()

	show := tmdb.GetShow(ID, config.Get().StrmLanguage)

	if show == nil {
		return nil, nil, errors.New("Unable to find show to remove")
	}

	paths := getShowPaths(show)

	if len(paths) == 0 {
		log.Warningf("Cannot find directories with strm files")
		return show, nil, errors.New("LOCALIZE[30282]")
	}
	ret := []string{}
	for path := range paths {
		if err := os.RemoveAll(path); err != nil {
			log.Error(err)
			return show, nil, err
		}

		ret = append(ret, path)
		log.Warningf("Directory %s removed from disk", path)
	}

	log.Warningf("%s removed from library", show.Name)

	return show, ret, nil
}

// RemoveEpisode removes episode from the library
func RemoveEpisode(tmdbID int, showID int, seasonNumber int, episodeNumber int) error {
	if err := checkShowsPath(); err != nil {
		return err
	}
	show := tmdb.GetShow(showID, config.Get().StrmLanguage)

	if show == nil {
		return errors.New("Unable to find show to remove episode")
	}

	showName := show.OriginalName
	if config.Get().StrmLanguage != config.Get().Language && show.Name != "" {
		showName = show.Name
	}

	showPath := util.ToFileName(fmt.Sprintf("%s (%s)", showName, strings.Split(show.FirstAirDate, "-")[0]))
	episodeStrm := fmt.Sprintf("%s S%02dE%02d.strm", showPath, seasonNumber, episodeNumber)
	episodePath := filepath.Join(ShowsLibraryPath(), showPath, episodeStrm)

	alreadyRemoved := false
	if _, err := os.Stat(episodePath); err != nil {
		alreadyRemoved = true
	}
	if !alreadyRemoved {
		if err := os.Remove(episodePath); err != nil {
			return err
		}
	}

	removedEpisodes <- &removedEpisode{
		ID:       tmdbID,
		ShowID:   showID,
		ShowName: show.Name,
		Season:   seasonNumber,
		Episode:  episodeNumber,
	}

	if !alreadyRemoved {
		log.Noticef("%s removed from library", episodeStrm)
	} else {
		return errors.New("Nothing left to remove from Elementum")
	}

	return nil
}

//
// Database updates
//

func updateDBItem(tmdbID int, state int, mediaType int, showID int) error {
	if tmdbID <= 0 {
		return fmt.Errorf("Cannot update DBItem due to missing TMDB ID")
	}

	defer perf.ScopeTimer()()

	li := database.LibraryItem{
		ID:        tmdbID,
		MediaType: mediaType,
		ShowID:    showID,
		State:     state,
	}
	if err := database.GetStormDB().Save(&li); err != nil {
		log.Debugf("updateDBItem failed: %s", err)
		return err
	}
	return nil
}

func updateBatchDBItem(tmdbIds []int, state int, mediaType int, showID int) error {
	defer perf.ScopeTimer()()

	tx, err := database.GetStormDB().Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, id := range tmdbIds {
		li := database.LibraryItem{
			ID:        id,
			MediaType: mediaType,
			ShowID:    showID,
			State:     state,
		}
		err = tx.Save(&li)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func deleteDBItem(tmdbID int, mediaType int, removal bool) error {
	defer perf.ScopeTimer()()

	var li database.LibraryItem
	if err := database.GetStormDB().One("ID", tmdbID, &li); err != nil {
		log.Debugf("Cannot find deleted item: %s", err)
		return err
	}

	if removal {
		li.State = StateDeleted
	} else {
		li.State = StateActive
	}

	if err := database.GetStormDB().Save(&li); err != nil {
		log.Debugf("Cannot update deleted item: %s", err)
		return err
	}

	return nil
}

// func deleteBatchDBItem(tmdbIds []int, mediaType int) error {
// 	// tx, err := database.Get().Begin()
// 	// if err != nil {
// 	// 	log.Debugf("Cannot start transaction: %s", err)
// 	// 	return err
// 	// }
// 	// for _, id := range tmdbIds {
// 	// 	_, err := tx.Exec(`UPDATE library_items SET state = ? WHERE tmdbId = ? AND mediaType = ?`, StateDeleted, id, mediaType)
// 	// 	if err != nil {
// 	// 		log.Debugf("deleteDBItem failed: %s", err)
// 	// 		tx.Rollback()
// 	// 		return err
// 	// 	}
// 	// }
// 	// tx.Commit()

// 	return nil
// }

func wasRemoved(id int, mediaType int) (wasRemoved bool) {
	defer perf.ScopeTimer()()

	var li database.LibraryItem
	if err := database.GetStormDB().Select(q.Eq("ID", id), q.Eq("MediaType", mediaType), q.Eq("State", StateDeleted)).First(&li); err == nil && li.ID != 0 {
		log.Debugf("mediaType=%d id=%d marked as removed in database", mediaType, id)
		return true
	}

	return false
}

//
// Maintenance
//

// CloseLibrary ...
func CloseLibrary() {
	log.Info("Closing library...")
	closer.Set()
}

// ClearPageCache deletes cached page listings
func ClearPageCache() {
	cacheDB := database.GetCache()
	if cacheDB != nil {
		cacheDB.DeleteWithPrefix(database.CommonBucket, []byte("page."))
	}
	xbmc.Refresh()
}

// ClearResolveCache deletes cached IDs resolve
func ClearResolveCache() {
	cacheDB := database.GetCache()
	if cacheDB != nil {
		cacheDB.DeleteWithPrefix(database.CommonBucket, []byte("Resolve_"))
	}
}

// ClearCacheKey deletes specific key
func ClearCacheKey(key string) {
	cacheDB := database.GetCache()
	if cacheDB != nil {
		log.Debugf("Removing cache key: %s", key)
		if err := cacheDB.Delete(database.CommonBucket, key); err != nil {
			log.Debugf("Error removing key from cache: %#v", err)
		}
	}
}

// ClearTraktCache deletes cached trakt data
func ClearTraktCache() {
	cacheDB := database.GetCache()
	if cacheDB != nil {
		cacheDB.DeleteWithPrefix(database.CommonBucket, []byte(cache.TraktKey))
	}
	xbmc.Refresh()
}

// ClearTmdbCache deletes cached tmdb data
func ClearTmdbCache() {
	cacheDB := database.GetCache()
	if cacheDB != nil {
		cacheDB.DeleteWithPrefix(database.CommonBucket, []byte(cache.TMDBKey))
	}
	xbmc.Refresh()
}

//
// Utilities
// 		mainly copied from api/routes to skip cycle imports

// URLForHTTP ...
func URLForHTTP(pattern string, args ...interface{}) string {
	u, _ := url.Parse(fmt.Sprintf(pattern, args...))
	return util.GetHTTPHost() + u.String()
}

// URLForXBMC ...
func URLForXBMC(pattern string, args ...interface{}) string {
	u, _ := url.Parse(fmt.Sprintf(pattern, args...))
	return "plugin://" + config.Get().Info.ID + u.String()
}

// URLQuery ...
func URLQuery(route string, query ...string) string {
	v := url.Values{}
	for i := 0; i < len(query); i += 2 {
		v.Add(query[i], query[i+1])
	}
	return route + "?" + v.Encode()
}

//
// Movie internals
//

// SyncMoviesList updates trakt movie collections in cache
func SyncMoviesList(listID string, updating bool, isUpdateNeeded bool) (err error) {
	if err = checkMoviesPath(); err != nil {
		return
	}

	started := time.Now()
	defer func() {
		log.Debugf("Trakt sync movies %s finished in %s", listID, time.Since(started))
	}()

	var label string
	var movies []*trakt.Movies

	switch listID {
	case "watchlist":
		movies, err = trakt.WatchlistMovies(isUpdateNeeded)
		label = "LOCALIZE[30254]"
	case "collection":
		movies, err = trakt.CollectionMovies(isUpdateNeeded)
		label = "LOCALIZE[30257]"
	default:
		movies, err = trakt.ListItemsMovies("", listID, isUpdateNeeded)
		label = "LOCALIZE[30263]"
	}

	if err != nil {
		log.Error(err)
		return
	}

	var movieIDs []int
	for _, movie := range movies {
		title := movie.Movie.Title
		// Try to resolve TMDB id through IMDB id, if provided
		if movie.Movie.IDs.TMDB == 0 && len(movie.Movie.IDs.IMDB) > 0 {
			r := tmdb.Find(movie.Movie.IDs.IMDB, "imdb_id")
			if r != nil && len(r.MovieResults) > 0 {
				movie.Movie.IDs.TMDB = r.MovieResults[0].ID
			}
		}

		if movie.Movie.IDs.TMDB == 0 {
			log.Warningf("Missing TMDB ID for %s", title)
			continue
		}

		tmdbID := strconv.Itoa(movie.Movie.IDs.TMDB)

		// FIXME: 'updating' is always passed as false, so wasRemoved check is always ignored.
		// also writeMovieStrm now has wasRemoved check.
		if updating && wasRemoved(movie.Movie.IDs.TMDB, MovieType) {
			continue
		}

		// FIXME: should it be like for shows - 'if !updating && !isUpdateNeeded && IsDuplicateShow(tmdbID) {' ?
		if uid.IsDuplicateMovie(tmdbID) {
			continue
		}

		if _, err := writeMovieStrm(tmdbID, false); err != nil {
			continue
		}

		movieIDs = append(movieIDs, movie.Movie.IDs.TMDB)
	}

	if err := updateBatchDBItem(movieIDs, StateActive, MovieType, 0); err != nil {
		return err
	}

	if !updating && len(movieIDs) > 0 {
		log.Noticef("Movies list (%s) added", listID)
		if config.Get().LibraryUpdate == 0 || (config.Get().LibraryUpdate == 1 && xbmc.DialogConfirmFocused("Elementum", fmt.Sprintf("LOCALIZE[30277];;%s", label))) {
			xbmc.VideoLibraryScan()
		}
	}
	return nil
}

//
// Shows internals
//

// SyncShowsList updates trakt collections in cache
func SyncShowsList(listID string, updating bool, isUpdateNeeded bool) (err error) {
	if err = checkShowsPath(); err != nil {
		return err
	}

	started := time.Now()
	defer func() {
		log.Debugf("Trakt sync shows %s finished in %s", listID, time.Since(started))
	}()

	var label string
	var shows []*trakt.Shows
	var previous []*trakt.Shows
	var current []*trakt.Shows

	switch listID {
	case "watchlist":
		previous, _ = trakt.PreviousWatchlistShows()
		current, _ = trakt.WatchlistShows(isUpdateNeeded)

		label = "LOCALIZE[30254]"
	case "collection":
		previous, _ = trakt.PreviousCollectionShows()
		current, _ = trakt.CollectionShows(isUpdateNeeded)

		label = "LOCALIZE[30257]"
	default:
		previous, _ = trakt.PreviousListItemsShows(listID)
		current, _ = trakt.ListItemsShows("", listID, isUpdateNeeded)

		label = "LOCALIZE[30263]"
	}

	// For first run we will try to write all shows, not only the delta
	if !IsTraktInitialized {
		shows = current
	} else {
		shows = DiffTraktShows(previous, current, IsTraktInitialized)
	}

	if err != nil {
		log.Error(err)
		return
	}

	cacheStore := cache.NewDBStore()
	showsLastUpdates := map[int]time.Time{}

	// Keep tracking of processed shows to avoid re-writing and checking all of them again.
	cacheStore.Get(cache.LibraryShowsLastUpdatesKey, &showsLastUpdates)
	defer func() {
		cacheStore.Set(cache.LibraryShowsLastUpdatesKey, &showsLastUpdates, cache.LibraryShowsLastUpdatesExpire)
	}()

	var showIDs []int
	for _, show := range shows {
		title := show.Show.Title
		// Try to resolve TMDB id through IMDB id, if provided
		if show.Show.IDs.TMDB == 0 {
			if len(show.Show.IDs.IMDB) > 0 {
				r := tmdb.Find(show.Show.IDs.IMDB, "imdb_id")
				if r != nil && len(r.TVResults) > 0 {
					show.Show.IDs.TMDB = r.TVResults[0].ID
				}
			}
			if show.Show.IDs.TMDB == 0 && show.Show.IDs.TVDB != 0 {
				r := tmdb.Find(strconv.Itoa(show.Show.IDs.TVDB), "tvdb_id")
				if r != nil && len(r.TVResults) > 0 {
					show.Show.IDs.TMDB = r.TVResults[0].ID
				}
			}
		}

		if show.Show.IDs.TMDB == 0 {
			log.Warningf("Missing TMDB ID for %s", title)
			continue
		}

		tmdbID := strconv.Itoa(show.Show.IDs.TMDB)
		if t, ok := showsLastUpdates[show.Show.IDs.Trakt]; ok && uid.IsDuplicateShow(tmdbID) && !t.Before(show.Show.UpdatedAt) {
			continue
		}
		showsLastUpdates[show.Show.IDs.Trakt] = show.Show.UpdatedAt

		if !updating && !isUpdateNeeded && uid.IsDuplicateShow(tmdbID) {
			continue
		}

		if _, err := writeShowStrm(show.Show.IDs.TMDB, false, false); err != nil {
			continue
		}

		showIDs = append(showIDs, show.Show.IDs.TMDB)
	}

	// Cleanup unused map items
	found := false
	for k := range showsLastUpdates {
		found = false
		for _, s := range shows {
			if s.Show.IDs.Trakt == k {
				found = true
				break
			}
		}

		if !found {
			delete(showsLastUpdates, k)
		}
	}

	if err := updateBatchDBItem(showIDs, StateActive, ShowType, 0); err != nil {
		return err
	}

	if !updating && len(showIDs) > 0 {
		log.Noticef("Shows list (%s) added", listID)
		if config.Get().LibraryUpdate == 0 || (config.Get().LibraryUpdate == 1 && xbmc.DialogConfirmFocused("Elementum", fmt.Sprintf("LOCALIZE[30277];;%s", label))) {
			xbmc.VideoLibraryScan()
		}
	}
	return nil
}

// DiffTraktShows ...
func DiffTraktShows(previous, current []*trakt.Shows, isInitialized bool) []*trakt.Shows {
	ret := make([]*trakt.Shows, 0, len(current))
	found := false
	for _, ce := range current {
		found = false
		for _, pr := range previous {
			if pr.Show.IDs.Trakt == ce.Show.IDs.Trakt {
				found = true
				break
			}
		}

		// If Trakt is not yet initialized - we can leave shows that are not yet in the library
		if !found || (!isInitialized && !uid.IsDuplicateShowByInt(ce.Show.IDs.TMDB)) {
			ret = append(ret, ce)
		}
	}

	return ret
}

//
// External handlers
//

// AddMovie is adding movie to the library
func AddMovie(tmdbID string, force bool) (*tmdb.Movie, error) {
	if err := checkMoviesPath(); err != nil {
		return nil, err
	}

	movie := tmdb.GetMovieByID(tmdbID, config.Get().Language)
	if movie == nil {
		return nil, fmt.Errorf("Movie with TMDB %s not found", tmdbID)
	}

	if !force && uid.IsDuplicateMovie(tmdbID) {
		xbmc.Notify("Elementum", fmt.Sprintf("LOCALIZE[30287];;%s", movie.Title), config.AddonIcon())
		return nil, fmt.Errorf("Movie already added")
	}

	if _, err := writeMovieStrm(tmdbID, force); err != nil {
		return movie, err
	}

	ID, _ := strconv.Atoi(tmdbID)
	if err := updateDBItem(ID, StateActive, MovieType, 0); err != nil {
		return movie, err
	}

	log.Noticef("%s added to library", movie.Title)
	return movie, nil
}

// AddShow is adding show to the library
func AddShow(tmdbID string, force bool) (*tmdb.Show, error) {
	if err := checkShowsPath(); err != nil {
		return nil, err
	}

	ID, _ := strconv.Atoi(tmdbID)
	show := tmdb.GetShowByID(tmdbID, config.Get().Language)

	if !force && uid.IsDuplicateShow(tmdbID) {
		xbmc.Notify("Elementum", fmt.Sprintf("LOCALIZE[30287];;%s", show.Name), config.AddonIcon())
		return show, fmt.Errorf("Show already added")
	}

	if err := updateDBItem(ID, StateActive, ShowType, ID); err != nil {
		return show, err
	}

	if _, err := writeShowStrm(ID, true, force); err != nil {
		log.Errorf("Error writing strm for a show: %s", err)
		return show, err
	}

	return show, nil
}

func getShowPath(show *tmdb.Show) (showPath, showStrm string) {
	// If this show already uses any directory - we should write there, to avoid having duplicates
	paths := getShowPathsByTMDB(show.ID)
	if len(paths) != 0 {
		for path := range paths {
			showPath = path
			break
		}
	}

	showName := show.OriginalName
	if config.Get().StrmLanguage != config.Get().Language && show.Name != "" {
		showName = show.Name
	}

	showStrm = util.ToFileName(fmt.Sprintf("%s (%s)", showName, strings.Split(show.FirstAirDate, "-")[0]))
	showPath = filepath.Join(ShowsLibraryPath(), showStrm)

	return
}

func getMoviePathsByTMDB(id int) (ret map[string]bool) {
	ret = map[string]bool{}

	if m, err := uid.GetMovieByTMDB(id); err == nil {
		if m != nil && m.File != "" && strings.HasSuffix(m.File, ".strm") {
			ret[filepath.Dir(m.File)] = true
		}
	}

	return
}

func getShowPathsByTMDB(id int) (ret map[string]bool) {
	ret = map[string]bool{}

	if s, err := uid.FindShowByTMDB(id); err == nil {
		for _, e := range s.Episodes {
			if e != nil && e.File != "" && strings.HasSuffix(e.File, ".strm") {
				ret[filepath.Dir(e.File)] = true
			}
		}
	}

	return
}

func getMoviePaths(movie *tmdb.Movie) map[string]bool {
	paths := getMoviePathsByTMDB(movie.ID)
	if len(paths) != 0 {
		return paths
	}

	titles := []string{movie.Title, movie.OriginalTitle}
	for _, t := range titles {
		movieStrm := util.ToFileName(fmt.Sprintf("%s (%s)", t, strings.Split(movie.ReleaseDate, "-")[0]))
		moviePath := filepath.Join(MoviesLibraryPath(), movieStrm)

		if _, err := os.Stat(moviePath); err == nil {
			paths[moviePath] = true
		}
	}

	return paths
}

func getShowPaths(show *tmdb.Show) map[string]bool {
	paths := getShowPathsByTMDB(show.ID)
	if len(paths) != 0 {
		return paths
	}

	titles := []string{show.Name, show.OriginalName}
	for _, t := range titles {
		showStrm := util.ToFileName(fmt.Sprintf("%s (%s)", t, strings.Split(show.FirstAirDate, "-")[0]))
		showPath := filepath.Join(ShowsLibraryPath(), showStrm)

		if _, err := os.Stat(showPath); err == nil {
			paths[showPath] = true
		}
	}

	return paths
}
