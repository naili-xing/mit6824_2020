package mr

import (
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"time"
)
import "log"
import "net/rpc"
import "hash/fnv"

// for sorting by key.
type ByKey []KeyValue

// for sorting by key.
func (a ByKey) Len() int           { return len(a) }
func (a ByKey) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByKey) Less(i, j int) bool { return a[i].Key < a[j].Key }
//
// Map functions return a slice of KeyValue.
//
type KeyValue struct {
	Key   string
	Value string
}

//
// use ihash(key) % NReduce to choose the reduce
// task number for each KeyValue emitted by Map.
//
func ihash(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32() & 0x7fffffff)
}

type MapF func(string, string) []KeyValue
type ReduceF func(string, []string) string

//
// main/mrworker.go calls this function.
//
func Worker(
	mapf MapF,
	reducef ReduceF,
	) {

	WorkerId := WorkerDefaultIndex

	for {
		args := TaskArgs{}
		reply := TaskReply{}
		args.WorkerId = WorkerId

		err := call("Master.Schedule", &args, &reply)
		if err != true {
			DPrintf("[Worker]: Worker %d Call Master Schedule error\n", WorkerId)
		}

		// if master return isFinish, break the loop
		if reply.IsFinish==true{
			DPrintf("[Worker]: Worker %d TaskDone. quit\n", WorkerId)
			return
		}
		WorkerId = reply.WorkerId
		switch reply.Phase {
		case MapPhrase:
			execMap(&reply, mapf)
		case ReducePhrase:
			execReduce(&reply, reducef)
		}
		/*
		Workers will sometimes need to wait, e.g. reduces can't start until the last map has finished.
		One possibility is for workers to periodically ask the master for work, sleeping with time.Sleep()
		between each request. Another possibility is for the relevant RPC handler in the master to have a
		loop that waits, either with time.Sleep() or sync.Cond. Go runs the handler for each RPC in its own
		thread, so the fact that one handler is waiting won't prevent the master from processing other RPCs.
		 */
		time.Sleep(time.Millisecond*10)
	}
}

//
// send an RPC request to the master, wait for the response.
// usually returns true.
// returns false if something goes wrong.
//
func call(rpcname string, args interface{}, reply interface{}) bool {
	// c, err := rpc.DialHTTP("tcp", "127.0.0.1"+":1234")
	sockname := masterSock()
	c, err := rpc.DialHTTP("unix", sockname)
	if err != nil {
		log.Fatal("dialing:", rpcname, err)
	}
	defer c.Close()

	err = c.Call(rpcname, args, reply)
	if err == nil {
		return true
	}

	fmt.Println(err)
	return false
}

func execMap(reply *TaskReply, mapf MapF){

	//DPrintf("[Worker]: Assigned Map, partitionF, x %d and nReduce %d, \n", reply.X, reply.RTasks)

	/*
	A worker who is assigned a map task reads the
	contents of the corresponding input split. It parses
	key/value pairs out of the input data and passes each
	pair to the user-defined Map function. The intermediate
	key/value pairs produced by the Map function
	are buffered in memory.
	 */

	filename := reply.FileName[0]
	content := readFile(filename)
	kva := mapf(filename, content)

	/*
	Periodically, the buffered pairs are written to local
	disk, partitioned into R regions by the partitioning
	function. The locations of these buffered pairs on
	the local disk are passed back to the master, who
	is responsible for forwarding these locations to the
	reduce workers.
	 */

	tmpFiles, realFiles := partitionF(kva, reply.X, reply.RTasks, reply.WorkerId, filename)

	xargs := TaskReportArgs{
		Phase:reply.Phase,
		WorkerId:reply.WorkerId,
		SplitId:reply.X,
		IntermediateFiles: realFiles,
		}

	// return the real nameFiles,
	xreply := TaskReportReply{}
	DPrintf("[Worker]: Worker %d After Map, Call Master.Collect to report status\n", reply.WorkerId)
	err := call("Master.Collect", &xargs, &xreply)
	if err != true {
		DPrintf("[Worker]: Worker %d After Map, Call Master.Collect error\n", reply.WorkerId)
	}

	//DPrintf("[Worker]: reply.Accept: %s \n", xreply.Accept)
	if xreply.Accept == true{
		for i, v := range tmpFiles{
			DPrintf("[Worker]: Worker %d Renaming %s to %s\n", reply.WorkerId, v, realFiles[i])
			err2 := os.Rename(v, realFiles[i])
			if err2 != nil {
				DPrintf("[Worker]: Worker %d Rename error: %s\n", reply.WorkerId, err2)
				panic(err2)
			}
		}
	}else{
		DPrintf("[Worker]: Worker %d Not Renaming %v \n", reply.WorkerId, tmpFiles)
	}

}

func partitionF(kva []KeyValue, x, nReduce, workerId int, filename string) (tmpFiles, realFiles []string) {
	/*
		The map part of your worker can use the ihash(key) function (in worker.go) to pick the reduce task for a
		given key.

		You can steal some code from mrsequential.go for reading Map input files, for sorting
		intermedate key/value pairs between the Map and Reduce, and for storing Reduce output in files.

	*/
	intermediate := make([][]KeyValue, nReduce)

	for _, kv := range kva{

		part := ihash(kv.Key) % nReduce
		intermediate[part] = append(intermediate[part], kv)
	}
	for i, v := range intermediate{
		sort.Sort(ByKey(v))

		f, e :=ioutil.TempFile("./", "tmpFile---")
		if e!=nil{
			panic(e)
		}
		tmpName := f.Name()
		realName := fmt.Sprintf(BaseDir+"mr-%d-%d", x, i)
		toJsonFile(tmpName, v)
		tmpFiles = append(tmpFiles, tmpName)
		realFiles = append(realFiles, realName)
	}
	DPrintf("[Worker]: Worker %d process split %s Saving %v to file: %v \n", workerId, filename, intermediate, tmpFiles)
	return tmpFiles, realFiles
}

func execReduce(reply *TaskReply,  reducef ReduceF){
	//DPrintf("[Worker]: Assigned Reduce \n")
	filenames := reply.FileName

	/*
	When a reduce worker is notified by the master
	about these locations, it uses remote procedure calls
	to read the buffered data from the local disks of the
	map workers. When a reduce worker has read all intermediate
	data, it sorts it by the intermediate keys
	so that all occurrences of the same key are grouped
	together. The sorting is needed because typically
	many different keys map to the same reduce task. If
	the amount of intermediate data is too large to fit in
	memory, an external sort is used.
	 */

	var intermediate []KeyValue
	for _, fname := range filenames{
		kva := Json2String(fname)
		intermediate = append(intermediate, kva...)
	}
	//DPrintf("[Worker]: Reduce Args: %v \n", intermediate)
	tmpFiles, realFiles, content := reduceHelper(intermediate, reducef, reply.Y)

	xargs := TaskReportArgs{
		Phase:reply.Phase,
		WorkerId:reply.WorkerId,
		SplitId:reply.Y,
	}
	xreply := TaskReportReply{}
	err := call("Master.Collect", &xargs, &xreply)

	DPrintf("[Worker]: Worker %d After Reduce, Call Master.Collect to report status\n", reply.WorkerId)

	if err != true {
		DPrintf("[Worker]: Worker %d After Reduce, Call Master Collect error\n", reply.WorkerId)
	}

	//DPrintf("[Worker]: reply.Accept: %s \n", xreply.Accept)
	if xreply.Accept == true{
		for i, v := range tmpFiles{
			DPrintf("[Worker]: Worker %d Renaming %s to %s, content: %v \n",reply.WorkerId, v, realFiles[i], content)
			err2 := os.Rename(v, realFiles[i])
			if err2 != nil {
				DPrintf("[Worker]: Worker %d Rename error: %s\n",reply.WorkerId, err2)
				panic(err2)
			}
		}
	}else{
		DPrintf("[Worker]: Worker %d Not Renaming %v \n", reply.WorkerId, tmpFiles)
	}

}

func reduceHelper(intermediate []KeyValue, reducef ReduceF, y int) (tmpFiles, realFiles,content []string) {

	ofile, e :=ioutil.TempFile("./", "outf---")
	if e!=nil{
		panic(e)
	}
	tmpName := ofile.Name()
	realName := fmt.Sprintf(BaseDir+"mr-out-%d",y)

	sort.Sort(ByKey(intermediate))

	/*
	The reduce worker iterates over the sorted intermediate
	data and for each unique intermediate key encountered,
	it passes the key and the corresponding
	set of intermediate values to the user’s Reduce function.
	The output of the Reduce function is appended
	to a final output file for this reduce partition
	 */

	i := 0
	for i < len(intermediate) {
		j := i + 1
		for j < len(intermediate) && intermediate[j].Key == intermediate[i].Key {
			j++
		}
		var values []string
		for k := i; k < j; k++ {
			values = append(values, intermediate[k].Value)
		}
		output := reducef(intermediate[i].Key, values)

		// this is the correct format for each line of Reduce output.
		fmt.Fprintf(ofile, "%v %v\n", intermediate[i].Key, output)
		content= append(content, fmt.Sprintf("%v %v", intermediate[i].Key, output))
		i = j
	}

	ofile.Close()
	tmpFiles = append(tmpFiles, tmpName)
	realFiles = append(realFiles, realName)

	return tmpFiles, realFiles, content
}