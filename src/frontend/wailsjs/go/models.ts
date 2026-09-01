export namespace main {
	
	export class DLLDetail {
	    relPath: string;
	    name: string;
	    version: string;
	
	    static createFrom(source: any = {}) {
	        return new DLLDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.relPath = source["relPath"];
	        this.name = source["name"];
	        this.version = source["version"];
	    }
	}
	export class GameDetails {
	    name: string;
	    path: string;
	    executable: string;
	    renderingAPI: string;
	    dlssVersion: string;
	    dlss5Addon: string;
	    reshadeStatus: string;
	    isInstalled: boolean;
	    dllList: DLLDetail[];
	
	    static createFrom(source: any = {}) {
	        return new GameDetails(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.executable = source["executable"];
	        this.renderingAPI = source["renderingAPI"];
	        this.dlssVersion = source["dlssVersion"];
	        this.dlss5Addon = source["dlss5Addon"];
	        this.reshadeStatus = source["reshadeStatus"];
	        this.isInstalled = source["isInstalled"];
	        this.dllList = this.convertValues(source["dllList"], DLLDetail);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GameInfo {
	    name: string;
	    path: string;
	    exePath: string;
	    isInstalled: boolean;
	    detectedAPI: string;
	
	    static createFrom(source: any = {}) {
	        return new GameInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.exePath = source["exePath"];
	        this.isInstalled = source["isInstalled"];
	        this.detectedAPI = source["detectedAPI"];
	    }
	}
	export class PatchResult {
	    success: boolean;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new PatchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.success = source["success"];
	        this.message = source["message"];
	    }
	}

}

