package main

func PluginConstructor(config map[string]interface{}) (*MevCollator, *MevCollatorAPI, error) {
    val, okay := config["maxMergedBundles"]
    if !okay {
        return nil, nil, errors.New("no field maxMergedBundles in config")
    }

    mmb, okay := val.(int)
    if !okay {
        return nil, nil, errors.New("field maxMergedBundles must be an integer")
    }

    // TODO some sanity check to make sure maxMergedBundles is a reasonable value

    maxMergedBundles := (uint)(mmb)

    collator := MevCollator{
        maxMergedBundles: maxMergedBundles,
        bundleMu: make(sync.Mutex),
        bundles: []MevBundle{},
    }

    api := NewMevCollatorAPI()

    return &collator, &api, nil
}
